package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/kubectl/pkg/cmd/get"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/apis/core"
	kprinters "k8s.io/kubernetes/pkg/printers"
	printersinternal "k8s.io/kubernetes/pkg/printers/internalversion"
)

func main() {
	cmd := cobra.Command{
		Use:   "selfctl",
		Short: "selfctl is demo cli",
		Long:  "selfctl is demo cli",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
		},
	}

	flags := cmd.PersistentFlags()
	flags.SetNormalizeFunc(cliflag.WarnWordSepNormalizeFunc) // Warn for "_" flags

	// Normalize all flags that are coming from other packages or pre-configurations
	// a.k.a. change all "_" to "-". e.g. glog package
	flags.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)

	kubeConfigFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	kubeConfigFlags.AddFlags(flags)
	matchVersionKubeConfigFlags := cmdutil.NewMatchVersionFlags(kubeConfigFlags)
	matchVersionKubeConfigFlags.AddFlags(cmd.PersistentFlags())

	cmd.PersistentFlags().AddGoFlagSet(flag.CommandLine)

	f := cmdutil.NewFactory(matchVersionKubeConfigFlags)

	cmd.AddCommand(NewGetCommand(f))

	err := cmd.Execute()
	if err != nil {
		return
	}
}

type GetOptions struct {
	PrintFlags *get.PrintFlags
	CmdParent  string

	resource.FilenameOptions

	Raw       string
	Watch     bool
	WatchOnly bool

	OutputWatchEvents bool

	LabelSelector     string
	FieldSelector     string
	AllNamespaces     bool
	Namespace         string
	ExplicitNamespace bool

	NoHeaders      bool
	Sort           bool
	IgnoreNotFound bool
	Export         bool

	genericclioptions.IOStreams
}

func NewOptions() *GetOptions {
	return &GetOptions{
		PrintFlags: get.NewGetPrintFlags(),
		IOStreams:  genericclioptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr},
	}
}

func NewGetCommand(f cmdutil.Factory) *cobra.Command {
	o := NewOptions()
	cmd := &cobra.Command{
		Use:   "get",
		Short: "get demo",
		Long:  "get demo",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.Complete(f, cmd, args); err != nil {
				return err
			}
			return o.Run(f, cmd, args)
		},
	}

	o.PrintFlags.AddFlags(cmd)
	cmd.Flags().StringVar(&o.Raw, "raw", o.Raw, "Raw URI to request from the server.  Uses the transport specified by the kubeconfig file.")
	cmd.Flags().BoolVarP(&o.Watch, "watch", "w", o.Watch, "After listing/getting the requested object, watch for changes. Uninitialized objects are excluded if no object name is provided.")
	cmd.Flags().BoolVar(&o.WatchOnly, "watch-only", o.WatchOnly, "Watch for changes to the requested object(s), without listing/getting first.")
	cmd.Flags().BoolVar(&o.OutputWatchEvents, "output-watch-events", o.OutputWatchEvents, "Output watch event objects when --watch or --watch-only is used. Existing objects are output as initial ADDED events.")
	cmd.Flags().BoolVar(&o.IgnoreNotFound, "ignore-not-found", o.IgnoreNotFound, "If the requested object does not exist the command will return exit code 0.")
	cmd.Flags().StringVarP(&o.LabelSelector, "selector", "l", o.LabelSelector, "Selector (label query) to filter on, supports '=', '==', and '!='.(e.g. -l key1=value1,key2=value2)")
	cmd.Flags().StringVar(&o.FieldSelector, "field-selector", o.FieldSelector, "Selector (field query) to filter on, supports '=', '==', and '!='.(e.g. --field-selector key1=value1,key2=value2). The server only supports a limited number of field queries per type.")
	cmd.Flags().BoolVarP(&o.AllNamespaces, "all-namespaces", "A", o.AllNamespaces, "If present, list the requested object(s) across all namespaces. Namespace in current context is ignored even if specified with --namespace.")
	cmd.Flags().BoolVar(&o.Export, "export", o.Export, "If true, use 'export' for the resources.  Exported resources are stripped of cluster-specific information.")
	cmd.Flags().MarkDeprecated("export", "This flag is deprecated and will be removed in future.")
	cmdutil.AddFilenameOptionFlags(cmd, &o.FilenameOptions, "identifying the resource to get from a server.")

	return cmd
}

func (o *GetOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error
	o.Namespace, o.ExplicitNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	if o.AllNamespaces {
		o.ExplicitNamespace = false
	}
	return nil
}

func (o *GetOptions) Run(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	r := f.NewBuilder().
		Unstructured().
		NamespaceParam(o.Namespace).DefaultNamespace().AllNamespaces(o.AllNamespaces).
		FilenameParam(o.ExplicitNamespace, &o.FilenameOptions).
		LabelSelectorParam(o.LabelSelector).
		FieldSelectorParam(o.FieldSelector).
		ExportParam(o.Export).
		ResourceTypeOrNameArgs(true, args...).
		ContinueOnError().
		//TransformRequests(o.transformRequests).
		Latest().
		Flatten().
		Do()

	infos, err := r.Infos()
	if err != nil {
		return err
	}

	generator := kprinters.NewTableGenerator().With(printersinternal.AddHandlers)

	// track if we write any output
	trackingWriter := &trackingWriterWrapper{Delegate: o.Out}
	// output an empty line separating output
	separatorWriter := &separatorWriterWrapper{Delegate: trackingWriter}
	w := printers.GetNewTabWriter(separatorWriter)

	allErrs := []error{}
	errs := sets.NewString()
	var printer kprinters.ResourcePrinter
	var lastMapping *meta.RESTMapping
	for _, info := range infos {
		mapping := info.Mapping
		if shouldGetNewPrinterForMapping(printer, lastMapping, mapping) {
			w.Flush()
			w.SetRememberedWidths(nil)

			// add linebreaks between resource groups (if there is more than one)
			// when it satisfies all following 3 conditions:
			// 1) it's not the first resource group
			// 2) it has row header
			// 3) we've written output since the last time we started a new set of headers
			if lastMapping != nil && !o.NoHeaders && trackingWriter.Written > 0 {
				separatorWriter.SetReady(true)
			}

			printer, err = o.PrintFlags.ToPrinter()
			if err != nil {
				if !errs.Has(err.Error()) {
					errs.Insert(err.Error())
					allErrs = append(allErrs, err)
				}
				continue
			}
			lastMapping = mapping
		}
		table, err := ConvertResource(generator, info.Object)
		if err != nil {
			return err
		}
		printer.PrintObj(table, w)
	}
	w.Flush()

	return utilerrors.NewAggregate(allErrs)
}

func printPod(pod *core.Pod, options kprinters.GenerateOptions) ([]metav1.TableRow, error) {
	return nil, nil
}

func shouldGetNewPrinterForMapping(printer printers.ResourcePrinter, lastMapping, mapping *meta.RESTMapping) bool {
	return printer == nil || lastMapping == nil || mapping == nil || mapping.Resource != lastMapping.Resource
}

func (o *GetOptions) transformRequests(req *rest.Request) {
	req.SetHeader("Accept", strings.Join([]string{
		fmt.Sprintf("application/json;as=Table;v=%s;g=%s", metav1.SchemeGroupVersion.Version, metav1.GroupName),
		fmt.Sprintf("application/json;as=Table;v=%s;g=%s", metav1beta1.SchemeGroupVersion.Version, metav1beta1.GroupName),
		"application/json",
	}, ","))

	// if sorting, ensure we receive the full object in order to introspect its fields via jsonpath
	if o.Sort {
		req.Param("includeObject", "Object")
	}
}

type trackingWriterWrapper struct {
	Delegate io.Writer
	Written  int
}

func (t *trackingWriterWrapper) Write(p []byte) (n int, err error) {
	t.Written += len(p)
	return t.Delegate.Write(p)
}

type separatorWriterWrapper struct {
	Delegate io.Writer
	Ready    bool
}

func (s *separatorWriterWrapper) Write(p []byte) (n int, err error) {
	// If we're about to write non-empty bytes and `s` is ready,
	// we prepend an empty line to `p` and reset `s.Read`.
	if len(p) != 0 && s.Ready {
		fmt.Fprintln(s.Delegate)
		s.Ready = false
	}
	return s.Delegate.Write(p)
}

func (s *separatorWriterWrapper) SetReady(state bool) {
	s.Ready = state
}

func ConvertResource(generator *kprinters.HumanReadableGenerator, obj runtime.Object) (*metav1.Table, error) {
	switch obj.GetObjectKind().GroupVersionKind().Kind {
	case "Deployment":
		v, ok := obj.(*apps.Deployment)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, apps.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*apps.Deployment)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "DeploymentList":
		v, ok := obj.(*apps.DeploymentList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, apps.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*apps.DeploymentList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Statefulset":
		v, ok := obj.(*apps.StatefulSet)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, apps.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*apps.StatefulSet)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "StatefulsetList":
		v, ok := obj.(*apps.StatefulSetList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, apps.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*apps.StatefulSetList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Pod":
		v, ok := obj.(*core.Pod)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.Pod)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "PodList":
		v, ok := obj.(*core.PodList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.PodList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Service":
		v, ok := obj.(*core.Service)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.Service)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "ServiceList":
		v, ok := obj.(*core.ServiceList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.ServiceList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Secret":
		v, ok := obj.(*core.Secret)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.Secret)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "SecretList":
		v, ok := obj.(*core.SecretList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.SecretList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Configmap":
		v, ok := obj.(*core.ConfigMap)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.ConfigMap)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "ConfigmapList":
		v, ok := obj.(*core.ConfigMapList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.ConfigMapList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "ReplicaSet":
		v, ok := obj.(*apps.ReplicaSet)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, apps.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*apps.ReplicaSet)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "ReplicaSetList":
		v, ok := obj.(*apps.ReplicaSetList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, apps.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*apps.ReplicaSetList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "DaemonSet":
		v, ok := obj.(*apps.DaemonSet)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, batch.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*apps.DaemonSet)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "DaemonSetList":
		v, ok := obj.(*apps.DaemonSetList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*apps.DaemonSetList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Job":
		v, ok := obj.(*batch.Job)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*batch.Job)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "JobList":
		v, ok := obj.(*batch.JobList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, batch.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*batch.JobList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "CronJob":
		v, ok := obj.(*batch.CronJob)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, batch.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*batch.CronJob)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "CronJobList":
		v, ok := obj.(*batch.CronJobList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, batch.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*batch.CronJobList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Endpoints":
		v, ok := obj.(*core.Endpoints)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.Endpoints)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "EndpointsList":
		v, ok := obj.(*core.EndpointsList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.EndpointsList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Node":
		v, ok := obj.(*core.Node)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.Node)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "NodeList":
		v, ok := obj.(*core.NodeList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.NodeList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Event":
		v, ok := obj.(*core.Event)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.Event)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "EventList":
		v, ok := obj.(*core.EventList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.EventList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Namespace":
		v, ok := obj.(*core.Namespace)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.Namespace)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "NamespaceList":
		v, ok := obj.(*core.NamespaceList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.NamespaceList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "ServiceAccount":
		v, ok := obj.(*core.ServiceAccount)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.ServiceAccount)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "ServiceAccountList":
		v, ok := obj.(*core.ServiceAccountList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.ServiceAccountList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Persistentvolume":
		v, ok := obj.(*core.PersistentVolume)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.PersistentVolume)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "PersistentvolumeList":
		v, ok := obj.(*core.PersistentVolumeList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.PersistentVolumeList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Persistentvolumeclaim":
		v, ok := obj.(*core.PersistentVolumeClaim)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.PersistentVolumeClaim)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "PersistentvolumeclaimList":
		v, ok := obj.(*core.PersistentVolumeClaimList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.PersistentVolumeClaimList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "Resourcequota":
		v, ok := obj.(*core.ResourceQuota)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.ResourceQuota)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	case "ResourcequotaList":
		v, ok := obj.(*core.ResourceQuotaList)
		if !ok {
			obj, err := legacyscheme.Scheme.ConvertToVersion(obj, core.SchemeGroupVersion)
			if err != nil {
				return nil, err
			}
			v = obj.(*core.ResourceQuotaList)
		}
		return generator.GenerateTable(v, kprinters.GenerateOptions{})
	default:
		return generator.GenerateTable(obj, kprinters.GenerateOptions{})
	}
}
