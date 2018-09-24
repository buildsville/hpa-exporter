package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/mitchellh/go-homedir"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	as_v1 "k8s.io/api/autoscaling/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
)

const (
	defaultMetricsInterval  = 30
	defaultConditionLogging = false
	defaultLoggingTo        = "stdout"
	defaultCWLogGroup       = "hpa-exporter"
	defaultCWLogStream      = "condition-log"
	defaultLoggingInterval  = 60
	defaultAddr             = ":9296"
)

const rootDoc = `<html>
<head><title>HPA Exporter</title></head>
<body>
<h1>HPA Exporter</h1>
<p><a href="/metrics">Metrics</a></p>
</body>
</html>
`

type currentMetrics struct {
	Type     string `json:"type"`
	Resource struct {
		Name                      string `json:"name"`
		CurrentAverageUtilization int    `json:"currentAverageUtilization"`
		CurrentAverageValue       string `json:"currentAverageValue"`
	} `json:"resource"`
}

type condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
}

const (
	conditionStatusTrue  string = "True"
	conditionStatusFalse string = "False"
)

var addr = flag.String("listen-address", defaultAddr, "The address to listen on for HTTP requests.")
var metricsInterval = flag.Int("metricsInterval", defaultMetricsInterval, "Interval to scrape HPA status.")
var loggingInterval = flag.Int("loggingInterval", defaultLoggingInterval, "Interval to logging HPA conditions.")
var conditionLogging = flag.Bool("conditionLogging", defaultConditionLogging, "Logging HPA conditions.")
var loggingTo = flag.String("loggingTo", defaultLoggingTo, "Where to log. (stdout or cwlogs)")
var cwLogGroup = flag.String("cwLogGroup", defaultCWLogGroup, "Name of CWLog group.")
var cwLogStream = flag.String("cwLogStream", defaultCWLogStream, "Name of CWLog stream.")

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

var cwSession = func() *cloudwatchlogs.CloudWatchLogs {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	return cloudwatchlogs.New(sess)
}()

var labels = []string{
	"hpa_name",
	"hpa_namespace",
	"ref_kind",
	"ref_name",
	"ref_apiversion",
}

var annoLabels = []string{
	"cond_status",
	"cond_reason",
	"cond_message",
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

	hpaCurrentCpuValue = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_current_cpu_value",
			Help: "Current cpu usage value.",
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

	hpaAbleToScale = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_able_to_scale",
			Help: "status able to scale from annotation.",
		},
		append(labels, annoLabels...),
	)

	hpaScalingActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_scaling_active",
			Help: "status scaling active from annotation.",
		},
		append(labels, annoLabels...),
	)

	hpaScalingLimited = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_scaling_limited",
			Help: "status scaling limited from annotation.",
		},
		append(labels, annoLabels...),
	)
)

func init() {
	prometheus.MustRegister(hpaCurrentPodsNum)
	prometheus.MustRegister(hpaDesiredPodsNum)
	prometheus.MustRegister(hpaMinPodsNum)
	prometheus.MustRegister(hpaMaxPodsNum)
	prometheus.MustRegister(hpaCurrentCpuValue)
	prometheus.MustRegister(hpaCurrentCpuPercentage)
	prometheus.MustRegister(hpaTargetCpuPercentage)
	prometheus.MustRegister(hpaLastScaleSecond)
	prometheus.MustRegister(hpaAbleToScale)
	prometheus.MustRegister(hpaScalingActive)
	prometheus.MustRegister(hpaScalingLimited)
}

func validateFlags() error {
	if !(*loggingTo == "stdout" || *loggingTo == "cwlogs") {
		return fmt.Errorf("invalid value `%s` of flag `loggingTo`, specify either `stdout` or `cwlogs`", *loggingTo)
	}
	return nil
}

func getHpaList() ([]as_v1.HorizontalPodAutoscaler, error) {
	out, err := kubeClient.AutoscalingV1().HorizontalPodAutoscalers("").List(meta_v1.ListOptions{})
	return out.Items, err
}

func currentAverageCpuValue(metrics currentMetrics) int {
	val := strings.Trim(metrics.Resource.CurrentAverageValue, "m")
	if i, e := strconv.ParseInt(val, 10, 64); e == nil {
		return int(i)
	} else {
		log.Errorln(e)
		return 0
	}
}

func mergeLabels(m1, m2 map[string]string) map[string]string {
	ans := map[string]string{}

	for k, v := range m1 {
		ans[k] = v
	}
	for k, v := range m2 {
		ans[k] = v
	}
	return (ans)
}

func makeAnnotationCondLabels(cond condition) (prometheus.Labels, prometheus.Labels) {
	labelForward := prometheus.Labels{
		"cond_status":  cond.Status,
		"cond_reason":  cond.Reason,
		"cond_message": cond.Message,
	}
	var statusReverse string
	if cond.Status == conditionStatusTrue {
		statusReverse = conditionStatusFalse
	} else {
		statusReverse = conditionStatusTrue
	}
	labelReverse := prometheus.Labels{
		"cond_status":  statusReverse,
		"cond_reason":  "",
		"cond_message": "",
	}

	return labelForward, labelReverse
}

func putHPAConditionToCWLog(hpa []as_v1.HorizontalPodAutoscaler) error {
	t, e := token()
	if e != nil {
		return e
	}
	cwevent := []*cloudwatchlogs.InputLogEvent{}
	timestamp := aws.Int64(time.Now().Unix() * 1000)
	for _, a := range hpa {
		s := hpaConditionJsonString(a)
		cwevent = append(cwevent, &cloudwatchlogs.InputLogEvent{
			Message:   aws.String(s),
			Timestamp: timestamp,
		})
	}
	putEvent := &cloudwatchlogs.PutLogEventsInput{
		LogEvents:     cwevent,
		LogGroupName:  cwLogGroup,
		LogStreamName: cwLogStream,
		SequenceToken: t,
	}
	//return contains only token `ret["NextSequenceToken"]`
	_, err := cwSession.PutLogEvents(putEvent)
	return err
}

func hpaConditionJsonString(hpa as_v1.HorizontalPodAutoscaler) string {
	condJsonStr := hpa.ObjectMeta.Annotations["autoscaling.alpha.kubernetes.io/conditions"]
	return `{"name":"` + hpa.ObjectMeta.Name + `","conditions":` + condJsonStr + `}`
}

func token() (token *string, err error) {
	input := &cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        cwLogGroup,
		LogStreamNamePrefix: cwLogStream,
	}
	x, err := cwSession.DescribeLogStreams(input)
	if err == nil {
		if len(x.LogStreams) == 0 {
			err = createStream()
		} else {
			token = x.LogStreams[0].UploadSequenceToken
		}
	}
	return
}

func checkLogGroup() error {
	input := &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: cwLogGroup,
	}
	if r, e := cwSession.DescribeLogGroups(input); e == nil {
		if len(r.LogGroups) == 0 {
			if e := createLogGroup(); e != nil {
				return e
			}
		}
	} else {
		return e
	}
	return nil
}

func createLogGroup() error {
	input := &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: cwLogGroup,
	}
	_, err := cwSession.CreateLogGroup(input)
	return err
}

func createStream() error {
	input := &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  cwLogGroup,
		LogStreamName: cwLogStream,
	}
	_, err := cwSession.CreateLogStream(input)
	return err
}

func main() {
	flag.Parse()
	e := validateFlags()
	if e != nil {
		panic(e)
	}
	time.Local, e = time.LoadLocation("Asia/Tokyo")
	if e != nil {
		time.Local = time.FixedZone("Asia/Tokyo", 9*60*60)
	}
	e = checkLogGroup()
	if e != nil {
		panic(e)
	}
	log.Info("start HPA exporter")

	if *conditionLogging {
		go func() {
			for {
				hpa, err := getHpaList()
				if err != nil {
					log.Errorln(err)
					continue
				}
				if *loggingTo == "cwlogs" {
					putHPAConditionToCWLog(hpa)
				} else {
					for _, a := range hpa {
						name := a.ObjectMeta.Name
						logtext := a.ObjectMeta.Annotations["autoscaling.alpha.kubernetes.io/conditions"]
						log.Infof("{\"name\":\"%s\",\"conditions\":%s}", name, logtext)
					}
				}
				time.Sleep(time.Duration(*loggingInterval) * time.Second)
			}
		}()
	}

	go func() {
		var (
			annoLabelAbleToScale       = map[string]prometheus.Labels{}
			annoLabelAbleToScaleRev    = map[string]prometheus.Labels{}
			annoLabelScalingActive     = map[string]prometheus.Labels{}
			annoLabelScalingActiveRev  = map[string]prometheus.Labels{}
			annoLabelScalingLimited    = map[string]prometheus.Labels{}
			annoLabelScalingLimitedRev = map[string]prometheus.Labels{}
		)
		for {
			hpa, err := getHpaList()
			if err != nil {
				log.Errorln(err)
				continue
			}
			for _, a := range hpa {
				label := prometheus.Labels{
					"hpa_name":       a.ObjectMeta.Name,
					"hpa_namespace":  a.ObjectMeta.Namespace,
					"ref_kind":       a.Spec.ScaleTargetRef.Kind,
					"ref_name":       a.Spec.ScaleTargetRef.Name,
					"ref_apiversion": a.Spec.ScaleTargetRef.APIVersion,
				}

				metrJsonStr := a.ObjectMeta.Annotations["autoscaling.alpha.kubernetes.io/current-metrics"]
				condJsonStr := a.ObjectMeta.Annotations["autoscaling.alpha.kubernetes.io/conditions"]
				var metrics []currentMetrics
				var conditions []condition
				if e := json.Unmarshal([]byte(metrJsonStr), &metrics); e != nil {
					log.Errorln(e)
					continue
				}
				if e := json.Unmarshal([]byte(condJsonStr), &conditions); e != nil {
					log.Errorln(e)
					continue
				}

				var cpuVal int
				if len(metrics) != 0 {
					cpuVal = currentAverageCpuValue(metrics[0])
				} else {
					cpuVal = 0
				}

				hpaCurrentPodsNum.With(label).Set(float64(a.Status.CurrentReplicas))
				hpaDesiredPodsNum.With(label).Set(float64(a.Status.DesiredReplicas))
				if a.Spec.MinReplicas != nil {
					hpaMinPodsNum.With(label).Set(float64(*a.Spec.MinReplicas))
				}
				hpaMaxPodsNum.With(label).Set(float64(a.Spec.MaxReplicas))
				hpaCurrentCpuValue.With(label).Set(float64(cpuVal))
				if a.Status.CurrentCPUUtilizationPercentage != nil {
					hpaCurrentCpuPercentage.With(label).Set(float64(*a.Status.CurrentCPUUtilizationPercentage))
				}
				if a.Spec.TargetCPUUtilizationPercentage != nil {
					hpaTargetCpuPercentage.With(label).Set(float64(*a.Spec.TargetCPUUtilizationPercentage))
				}
				if a.Status.LastScaleTime != nil {
					hpaLastScaleSecond.With(label).Set(float64(a.Status.LastScaleTime.Unix()))
				}

				name := a.ObjectMeta.Name
				for _, cond := range conditions {
					switch cond.Type {
					case "AbleToScale":
						oldLabel := annoLabelAbleToScale[name]
						oldLabelRev := annoLabelAbleToScaleRev[name]

						annoLabelAbleToScale[name], annoLabelAbleToScaleRev[name] = makeAnnotationCondLabels(cond)

						_ = hpaAbleToScale.Delete(mergeLabels(label, oldLabel))
						_ = hpaAbleToScale.Delete(mergeLabels(label, oldLabelRev))
						hpaAbleToScale.With(mergeLabels(label, annoLabelAbleToScale[name])).Set(float64(1))
						hpaAbleToScale.With(mergeLabels(label, annoLabelAbleToScaleRev[name])).Set(float64(0))
					case "ScalingActive":
						oldLabel := annoLabelScalingActive[name]
						oldLabelRev := annoLabelScalingActiveRev[name]

						annoLabelScalingActive[name], annoLabelScalingActiveRev[name] = makeAnnotationCondLabels(cond)

						_ = hpaScalingActive.Delete(mergeLabels(label, oldLabel))
						_ = hpaScalingActive.Delete(mergeLabels(label, oldLabelRev))
						hpaScalingActive.With(mergeLabels(label, annoLabelScalingActive[name])).Set(float64(1))
						hpaScalingActive.With(mergeLabels(label, annoLabelScalingActiveRev[name])).Set(float64(0))
					case "ScalingLimited":
						oldLabel := annoLabelScalingLimited[name]
						oldLabelRev := annoLabelScalingLimitedRev[name]

						annoLabelScalingLimited[name], annoLabelScalingLimitedRev[name] = makeAnnotationCondLabels(cond)

						_ = hpaScalingLimited.Delete(mergeLabels(label, oldLabel))
						_ = hpaScalingLimited.Delete(mergeLabels(label, oldLabelRev))
						hpaScalingLimited.With(mergeLabels(label, annoLabelScalingLimited[name])).Set(float64(1))
						hpaScalingLimited.With(mergeLabels(label, annoLabelScalingLimitedRev[name])).Set(float64(0))
					}
				}
			}
			time.Sleep(time.Duration(*metricsInterval) * time.Second)
		}
	}()
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(rootDoc))
	})

	log.Fatal(http.ListenAndServe(*addr, nil))
}
