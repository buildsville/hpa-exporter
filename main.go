package main

import (
	"flag"
	"github.com/mitchellh/go-homedir"
	"net/http"
	"os"
	"time"

	as_v1 "k8s.io/api/autoscaling/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
)

const (
	defaultInterval = 30
	defaultPort     = ":9296"
)

const rootDoc = `<html>
<head><title>HPA Exporter</title></head>
<body>
<h1>HPA Exporter</h1>
<p><a href="/metrics">Metrics</a></p>
</body>
</html>
`

var addr = flag.String("listen-address", defaultPort, "The address to listen on for HTTP requests.")
var interval = flag.Int("interval", defaultInterval, "Interval to scrape HPA status.")

var kubeClient = func() kubernetes.Interface {
	var ret kubernetes.Interface
	config, err := rest.InClusterConfig()
	if err != nil {
		var kubeconfigPath string
		if os.Getenv("KUBECONFIG") == "" {
			home, err := homedir.Dir()
			if err != nil {
				panic(err)
			}
			kubeconfigPath = home + "/.kube/config"
		} else {
			kubeconfigPath = os.Getenv("KUBECONFIG")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			panic(err)
		}
	}
	ret, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}
	return ret
}()

var labels = []string{
	"hpa_name",
	"hpa_namespace",
	"ref_kind",
	"ref_name",
	"ref_apiversion",
}

var (
	hpaCurrentPodsNum = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_current_pods_num",
			Help: "Number of current pods by status.",
		},
		labels,
	)

	hpaDesiredPodsNum = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_desired_pods_num",
			Help: "Number of desired pods by status.",
		},
		labels,
	)

	hpaMinPodsNum = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_min_pods_num",
			Help: "Number of min pods by spec.",
		},
		labels,
	)

	hpaMaxPodsNum = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_max_pods_num",
			Help: "Number of max pods by spec.",
		},
		labels,
	)

	hpaCurrentCpuPercentage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_current_cpu_percentage",
			Help: "Current cpu utilization calculated by HPA.",
		},
		labels,
	)

	hpaTargetCpuPercentage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_target_cpu_percentage",
			Help: "Target CPU utilization set for HPA.",
		},
		labels,
	)

	hpaLastScaleSecond = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_last_scale_second",
			Help: "Time the scale was last executed.",
		},
		labels,
	)
)

func init() {
	prometheus.MustRegister(hpaCurrentPodsNum)
	prometheus.MustRegister(hpaDesiredPodsNum)
	prometheus.MustRegister(hpaMinPodsNum)
	prometheus.MustRegister(hpaMaxPodsNum)
	prometheus.MustRegister(hpaCurrentCpuPercentage)
	prometheus.MustRegister(hpaTargetCpuPercentage)
	prometheus.MustRegister(hpaLastScaleSecond)
}

func getHpaList() ([]as_v1.HorizontalPodAutoscaler, error) {
	out, err := kubeClient.AutoscalingV1().HorizontalPodAutoscalers("").List(meta_v1.ListOptions{})
	return out.Items, err
}

func main() {
	flag.Parse()
	log.Info("start HPA exporter")

	go func() {
		for {
			hpa, err := getHpaList()
			if err != nil {
				log.Errorln(err)
				continue
			}
			for _, a := range hpa {
				if a.Spec.MinReplicas == nil || a.Status.CurrentCPUUtilizationPercentage == nil || a.Spec.TargetCPUUtilizationPercentage == nil {
					continue
				}
				label := prometheus.Labels{
					"hpa_name":       a.ObjectMeta.Name,
					"hpa_namespace":  a.ObjectMeta.Namespace,
					"ref_kind":       a.Spec.ScaleTargetRef.Kind,
					"ref_name":       a.Spec.ScaleTargetRef.Name,
					"ref_apiversion": a.Spec.ScaleTargetRef.APIVersion,
				}
				hpaCurrentPodsNum.With(label).Set(float64(a.Status.CurrentReplicas))
				hpaDesiredPodsNum.With(label).Set(float64(a.Status.DesiredReplicas))
				hpaMinPodsNum.With(label).Set(float64(*a.Spec.MinReplicas))
				hpaMaxPodsNum.With(label).Set(float64(a.Spec.MaxReplicas))
				hpaCurrentCpuPercentage.With(label).Set(float64(*a.Status.CurrentCPUUtilizationPercentage))
				hpaTargetCpuPercentage.With(label).Set(float64(*a.Spec.TargetCPUUtilizationPercentage))
				hpaLastScaleSecond.With(label).Set(float64(a.Status.LastScaleTime.Unix()))
			}
			time.Sleep(time.Duration(*interval) * time.Second)
		}
	}()
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(rootDoc))
	})

	log.Fatal(http.ListenAndServe(*addr, nil))
}
