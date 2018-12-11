package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/mitchellh/go-homedir"
	"net/http"
	"os"
	"time"

	as_v1 "k8s.io/api/autoscaling/v1"
	as_v2 "k8s.io/api/autoscaling/v2beta1"
	core_v1 "k8s.io/api/core/v1"
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

type conditions struct {
	Name       string                                   `json:"name"`
	Conditions []as_v2.HorizontalPodAutoscalerCondition `json:"conditions"`
}

type commonMetrics struct {
	Kind       string
	Name       string
	MetricName string
	Value      float64
}

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

var baseLabels = []string{
	"hpa_name",
	"hpa_namespace",
	"ref_kind",
	"ref_name",
	"ref_apiversion",
}

var metricLabels = []string{
	"metric_kind",
	"metric_name",
	"metric_metricname",
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
		baseLabels,
	)

	hpaDesiredPodsNum = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_desired_pods_num",
			Help: "Number of desired pods by status.",
		},
		baseLabels,
	)

	hpaMinPodsNum = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_min_pods_num",
			Help: "Number of min pods by spec.",
		},
		baseLabels,
	)

	hpaMaxPodsNum = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_max_pods_num",
			Help: "Number of max pods by spec.",
		},
		baseLabels,
	)

	hpaLastScaleSecond = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_last_scale_second",
			Help: "Time the scale was last executed.",
		},
		baseLabels,
	)

	hpaCurrentMetricsValue = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_current_metrics_value",
			Help: "Current Metrics Value.",
		},
		append(baseLabels, metricLabels...),
	)

	hpaTargetMetricsValue = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_target_metrics_value",
			Help: "Target Metrics Value.",
		},
		append(baseLabels, metricLabels...),
	)

	hpaAbleToScale = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_able_to_scale",
			Help: "status able to scale from annotation.",
		},
		append(baseLabels, annoLabels...),
	)

	hpaScalingActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_scaling_active",
			Help: "status scaling active from annotation.",
		},
		append(baseLabels, annoLabels...),
	)

	hpaScalingLimited = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hpa_scaling_limited",
			Help: "status scaling limited from annotation.",
		},
		append(baseLabels, annoLabels...),
	)
)

var collectors = []prometheus.Collector{
	hpaCurrentPodsNum,
	hpaDesiredPodsNum,
	hpaMinPodsNum,
	hpaMaxPodsNum,
	hpaLastScaleSecond,
	hpaCurrentMetricsValue,
	hpaTargetMetricsValue,
	hpaAbleToScale,
	hpaScalingActive,
	hpaScalingLimited,
}

func init() {
	prometheus.MustRegister(collectors...)
}

func resetAllMetric(){
	for _,c := range collectors{
		if v,ok := c.(*prometheus.GaugeVec); ok {
			v.Reset()
		}
	}
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

func getHpaListV2() ([]as_v2.HorizontalPodAutoscaler, error) {
	out, err := kubeClient.AutoscalingV2beta1().HorizontalPodAutoscalers("").List(meta_v1.ListOptions{})
	return out.Items, err
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

func makeAnnotationCondLabels(cond as_v2.HorizontalPodAutoscalerCondition) (prometheus.Labels, prometheus.Labels) {
	labelForward := prometheus.Labels{
		"cond_status":  fmt.Sprintf("%v", cond.Status),
		"cond_reason":  cond.Reason,
		"cond_message": cond.Message,
	}
	var statusReverse string
	if cond.Status == core_v1.ConditionTrue {
		statusReverse = fmt.Sprintf("%v", core_v1.ConditionFalse)
	} else {
		statusReverse = fmt.Sprintf("%v", core_v1.ConditionTrue)
	}
	labelReverse := prometheus.Labels{
		"cond_status":  statusReverse,
		"cond_reason":  "",
		"cond_message": "",
	}

	return labelForward, labelReverse
}

func parseObjectSpec(m *as_v2.ObjectMetricSource) commonMetrics {
	return commonMetrics{
		Kind:       m.Target.Kind,
		Name:       m.Target.Name,
		MetricName: m.MetricName,
		Value:      float64(m.TargetValue.MilliValue()) / 1000,
	}
}

func parsePodsSpec(m *as_v2.PodsMetricSource) commonMetrics {
	return commonMetrics{
		Kind:       "Pod",
		Name:       "-",
		MetricName: m.MetricName,
		Value:      float64(m.TargetAverageValue.MilliValue()) / 1000,
	}
}

func parseResourceSpec(m *as_v2.ResourceMetricSource) commonMetrics {
	var t float64
	if m.TargetAverageUtilization == nil {
		t = float64(m.TargetAverageValue.MilliValue()) / 1000
	} else {
		t = float64(*m.TargetAverageUtilization)
	}
	return commonMetrics{
		Kind:       "Resource",
		Name:       m.Name.String(),
		MetricName: "-",
		Value:      t,
	}
}

func parseExternalSpec(m *as_v2.ExternalMetricSource) commonMetrics {
	var t float64
	if m.TargetAverageValue == nil {
		t = float64(m.TargetValue.MilliValue()) / 1000
	} else {
		t = float64(m.TargetAverageValue.MilliValue()) / 1000
	}
	return commonMetrics{
		Kind:       "External",
		Name:       "-",
		MetricName: m.MetricName,
		Value:      t,
	}
}

func parseObjectStatus(m *as_v2.ObjectMetricStatus) commonMetrics {
	return commonMetrics{
		Kind:       m.Target.Kind,
		Name:       m.Target.Name,
		MetricName: m.MetricName,
		Value:      float64(m.CurrentValue.MilliValue()) / 1000,
	}
}

func parsePodsStatus(m *as_v2.PodsMetricStatus) commonMetrics {
	return commonMetrics{
		Kind:       "Pod",
		Name:       "-",
		MetricName: m.MetricName,
		Value:      float64(m.CurrentAverageValue.MilliValue()) / 1000,
	}
}

func parseResourceStatus(m *as_v2.ResourceMetricStatus) commonMetrics {
	var t float64
	if m.CurrentAverageUtilization == nil {
		t = float64(m.CurrentAverageValue.MilliValue()) / 1000
	} else {
		t = float64(*m.CurrentAverageUtilization)
	}
	return commonMetrics{
		Kind:       "Resource",
		Name:       m.Name.String(),
		MetricName: "-",
		Value:      t,
	}
}

func parseExternalStatus(m *as_v2.ExternalMetricStatus) commonMetrics {
	var t float64
	if m.CurrentAverageValue == nil {
		t = float64(m.CurrentValue.MilliValue()) / 1000
	} else {
		t = float64(m.CurrentAverageValue.MilliValue()) / 1000
	}
	return commonMetrics{
		Kind:       "External",
		Name:       "-",
		MetricName: m.MetricName,
		Value:      t,
	}
}

func parseCommonMetrics(m commonMetrics) (float64, prometheus.Labels) {
	return m.Value, prometheus.Labels{
		"metric_kind":       m.Kind,
		"metric_name":       m.Name,
		"metric_metricname": m.MetricName,
	}
}

func putHPAConditionToCWLog(hpa []as_v2.HorizontalPodAutoscaler) error {
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

func hpaConditionJsonString(hpa as_v2.HorizontalPodAutoscaler) string {
	cond := conditions{
		Name:       hpa.ObjectMeta.Name,
		Conditions: hpa.Status.Conditions,
	}
	jsonBytes, err := json.Marshal(cond)
	if err != nil {
		fmt.Println("JSON Marshal error:", err)
		return "{}"
	}
	return string(jsonBytes)
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

	if *conditionLogging {
		e = checkLogGroup()
		if e != nil {
			panic(e)
		}
	}

	log.Info("start HPA exporter")

	if *conditionLogging {
		go func() {
			for {
				hpa, err := getHpaListV2()
				if err != nil {
					log.Errorln(err)
					continue
				}
				if *loggingTo == "cwlogs" {
					putHPAConditionToCWLog(hpa)
				} else {
					for _, a := range hpa {
						log.Infoln(hpaConditionJsonString(a))
					}
				}
				time.Sleep(time.Duration(*loggingInterval) * time.Second)
			}
		}()
	}

	go func() {
		for {
			hpa, err := getHpaListV2()
			if err != nil {
				log.Errorln(err)
				continue
			}
			resetAllMetric()
			for _, a := range hpa {
				baseLabel := prometheus.Labels{
					"hpa_name":       a.ObjectMeta.Name,
					"hpa_namespace":  a.ObjectMeta.Namespace,
					"ref_kind":       a.Spec.ScaleTargetRef.Kind,
					"ref_name":       a.Spec.ScaleTargetRef.Name,
					"ref_apiversion": a.Spec.ScaleTargetRef.APIVersion,
				}

				hpaCurrentPodsNum.With(baseLabel).Set(float64(a.Status.CurrentReplicas))
				hpaDesiredPodsNum.With(baseLabel).Set(float64(a.Status.DesiredReplicas))
				if a.Spec.MinReplicas != nil {
					hpaMinPodsNum.With(baseLabel).Set(float64(*a.Spec.MinReplicas))
				}
				hpaMaxPodsNum.With(baseLabel).Set(float64(a.Spec.MaxReplicas))
				if a.Status.LastScaleTime != nil {
					hpaLastScaleSecond.With(baseLabel).Set(float64(a.Status.LastScaleTime.Unix()))
				}

				for _, metric := range a.Spec.Metrics {
					switch metric.Type {
					case as_v2.ObjectMetricSourceType:
						m := parseObjectSpec(metric.Object)
						v, l := parseCommonMetrics(m)
						hpaTargetMetricsValue.With(mergeLabels(baseLabel, l)).Set(v)
					case as_v2.PodsMetricSourceType:
						m := parsePodsSpec(metric.Pods)
						v, l := parseCommonMetrics(m)
						hpaTargetMetricsValue.With(mergeLabels(baseLabel, l)).Set(v)
					case as_v2.ResourceMetricSourceType:
						m := parseResourceSpec(metric.Resource)
						v, l := parseCommonMetrics(m)
						hpaTargetMetricsValue.With(mergeLabels(baseLabel, l)).Set(v)
					case as_v2.ExternalMetricSourceType:
						m := parseExternalSpec(metric.External)
						v, l := parseCommonMetrics(m)
						hpaTargetMetricsValue.With(mergeLabels(baseLabel, l)).Set(v)
					default:
						continue
					}
				}

				for _, metric := range a.Status.CurrentMetrics {
					switch metric.Type {
					case as_v2.ObjectMetricSourceType:
						m := parseObjectStatus(metric.Object)
						v, l := parseCommonMetrics(m)
						hpaCurrentMetricsValue.With(mergeLabels(baseLabel, l)).Set(v)
					case as_v2.PodsMetricSourceType:
						m := parsePodsStatus(metric.Pods)
						v, l := parseCommonMetrics(m)
						hpaCurrentMetricsValue.With(mergeLabels(baseLabel, l)).Set(v)
					case as_v2.ResourceMetricSourceType:
						m := parseResourceStatus(metric.Resource)
						v, l := parseCommonMetrics(m)
						hpaCurrentMetricsValue.With(mergeLabels(baseLabel, l)).Set(v)
					case as_v2.ExternalMetricSourceType:
						m := parseExternalStatus(metric.External)
						v, l := parseCommonMetrics(m)
						hpaCurrentMetricsValue.With(mergeLabels(baseLabel, l)).Set(v)
					default:
						continue
					}
				}

				for _, cond := range a.Status.Conditions {
					annoLabel, annoLabelRev := makeAnnotationCondLabels(cond)
					switch cond.Type {
					case as_v2.AbleToScale:
						hpaAbleToScale.With(mergeLabels(baseLabel, annoLabel)).Set(float64(1))
						hpaAbleToScale.With(mergeLabels(baseLabel, annoLabelRev)).Set(float64(0))
					case as_v2.ScalingActive:
						hpaScalingActive.With(mergeLabels(baseLabel, annoLabel)).Set(float64(1))
						hpaScalingActive.With(mergeLabels(baseLabel, annoLabelRev)).Set(float64(0))
					case as_v2.ScalingLimited:
						hpaScalingLimited.With(mergeLabels(baseLabel, annoLabel)).Set(float64(1))
						hpaScalingLimited.With(mergeLabels(baseLabel, annoLabelRev)).Set(float64(0))
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
