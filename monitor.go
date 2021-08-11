package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containers/podman/v3/pkg/bindings"
	"github.com/containers/podman/v3/pkg/bindings/containers"
	"github.com/containers/podman/v3/pkg/bindings/images"
	"github.com/containers/podman/v3/pkg/specgen"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"

	"github.com/coreos/pkg/flagutil"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	log "github.com/sirupsen/logrus"
)

var listen = flag.String("listen", ":8000", "")
var level = flag.String("loglevel", "info", "default log level: debug, info, warn, error, fatal, panic")
var username = flag.String("username", "", "Registry username for pulling and pushing")
var password = flag.String("password", "", "Registry password for pulling and pushing")
var registryHost = flag.String("registry-host", "", "Hostname of the registry being monitored")
var repository = flag.String("repository", "", "Repository on the registry to pull and push")
var baseImage = flag.String("base-image", "", "Repository to use as base image for push image; instead of base-layer-id")
var publicBase = flag.Bool("public-base", false, "Is the base image public or private (default: false)")
var baseLayer = flag.String("base-layer-id", "", "Docker V1 ID of the base layer in the repository; instead of base-image")
var testInterval = flag.String("run-test-every", "2m", "the time between test in minutes")

var awsAccessKey = flag.String("aws-access-key", "", "AWS Access Key for connecting to CloudWatch")
var awsSecretKey = flag.String("aws-secret-key", "", "AWS Secret Key for connecting to CloudWatch")
var cloudwatchRegion = flag.String("cloudwatch-region", "us-east-1", "Region in which to write the CloudWatch metrics")
var cloudwatchNamespace = flag.String("cloudwatch-namespace", "", "Namespace in which to write the CloudWatch metrics")
var cloudwatchSuccessMetric = flag.String("cloudwatch-metric-success", "MonitorSuccess", "Name of the CloudWatch metric for successful operations")
var cloudwatchFailureMetric = flag.String("cloudwatch-metric-failure", "MonitorFailure", "Name of the CloudWatch metric for successful operations")
var cloudwatchPullTimeMetric = flag.String("cloudwatch-metric-pull-time", "MonitorPullTime", "Name of the CloudWatch metric for pull timing")
var cloudwatchPushTimeMetric = flag.String("cloudwatch-metric-push-time", "MonitorPushTime", "Name of the CloudWatch metric for push timing")

var (
	base         string
	dockerClient *docker.Client
	dockerHost   string
	healthy      bool
	status       bool
	podmanContext context.Context
)

var (
	promNamespace = os.Getenv("PROMETHEUS_NAMESPACE")

	promSuccessMetric = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: promNamespace,
		Subsystem: "",
		Name:      "monitor_success",
		Help:      "The registry monitor successfully completed a pull and push operation",
	}, []string{})

	promFailureMetric = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: promNamespace,
		Subsystem: "",
		Name:      "monitor_failure",
		Help:      "The registry monitor failed to complete a pull and push operation",
	}, []string{})

	promPushMetric = prometheus.NewSummary(prometheus.SummaryOpts{
		Namespace: promNamespace,
		Subsystem: "",
		Name:      "monitor_push",
		Help:      "The time for the monitor push operation",
	})

	promPullMetric = prometheus.NewSummary(prometheus.SummaryOpts{
		Namespace: promNamespace,
		Subsystem: "",
		Name:      "monitor_pull",
		Help:      "The time for the monitor pull operation",
	})
)

var prometheusMetrics = []prometheus.Collector{promSuccessMetric, promFailureMetric, promPullMetric, promPushMetric}

type LoggingWriter struct{}

func (w *LoggingWriter) Write(p []byte) (n int, err error) {
	s := string(p)
	log.Infof("%s", s)
	return len(s), nil
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if !healthy {
		w.WriteHeader(503)
	}

	fmt.Fprintf(w, "%t", healthy)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	if !status {
		w.WriteHeader(400)
	}

	fmt.Fprintf(w, "%t", status)
}

func buildTLSTransport(basePath string) (*http.Transport, error) {
	roots := x509.NewCertPool()
	pemData, err := ioutil.ReadFile(filepath.Join(basePath, "ca.pem"))
	if err != nil {
		return nil, err
	}

	// Add the certification to the pool.
	roots.AppendCertsFromPEM(pemData)

	// Create the certificate.
	crt, err := tls.LoadX509KeyPair(filepath.Join(basePath, "/cert.pem"), filepath.Join(basePath, "/key.pem"))
	if err != nil {
		return nil, err
	}

	// Create the new tls configuration using both the authority and certificate.
	conf := &tls.Config{
		RootCAs:      roots,
		Certificates: []tls.Certificate{crt},
	}

	// Create our own transport and return it.
	return &http.Transport{
		TLSClientConfig: conf,
	}, nil
}

func newPodmanClient() (context.Context, error) {

	socket := "ssh://vagrant@127.0.0.1:2222/run/user/1000/podman/podman.sock"
	absPath, _ := filepath.Abs("opensshkey")

	podmanContext, err := bindings.NewConnectionWithIdentity(context.Background(), socket, absPath)
	if err != nil {
		return nil, err
	}

	return podmanContext, nil
}

func stringInSlice(value string, list []string) bool {
	for _, current := range list {
		if current == value {
			return true
		}
	}
	return false
}

func clearAllContainers(dockerClient *docker.Client) bool {
	listOptions := docker.ListContainersOptions{
		All: true,
	}

	log.Infof("Listing all containers")
	containers, err := dockerClient.ListContainers(listOptions)
	if err != nil {
		log.Errorf("Error listing containers: %s", err)
		return false
	}

	for _, container := range containers {
		if stringInSlice("monitor", container.Names) {
			continue
		}

		log.Infof("Removing container: %s", container.ID)
		removeOptions := docker.RemoveContainerOptions{
			ID:            container.ID,
			RemoveVolumes: true,
			Force:         true,
		}

		if err = dockerClient.RemoveContainer(removeOptions); err != nil {
			log.Errorf("Error removing container: %s", err)
			return false
		}
	}

	return true
}

func clearAllImages(dockerClient *docker.Client) bool {
	// Note: We delete in a loop like this because deleting one
	// image can lead to others being deleted. Therefore, we just
	// loop until the images list are empty.

	skipImages := map[string]bool{}

	for {
		// List all Docker images.
		listOptions := docker.ListImagesOptions{
			All: true,
		}

		log.Infof("Listing docker images")
		images, err := dockerClient.ListImages(listOptions)
		if err != nil {
			log.Errorf("Could not list images: %s", err)
			return false
		}

		// Determine if we need to remove any images.
		imagesFound := false
		for _, image := range images {
			if _, toSkip := skipImages[image.ID]; toSkip {
				continue
			}

			imagesFound = true
		}

		if !imagesFound {
			return true
		}

		// Remove images.
		removedImages := false
		for _, image := range images[:1] {
			if _, toSkip := skipImages[image.ID]; toSkip {
				continue
			}

			log.Infof("Clearing image %s", image.ID)
			if err = dockerClient.RemoveImage(image.ID); err != nil {
				if strings.ToLower(os.Getenv("UNDER_DOCKER")) != "true" {
					log.Errorf("%s", err)
					return false
				} else {
					log.Warningf("Skipping deleting image %v", image.ID)
					skipImages[image.ID] = true
					continue
				}
			}

			removedImages = true
		}

		if !removedImages {
			break
		}
	}

	return true
}

func pullTestImage(podmanContext context.Context) bool {
	fullImagePath := imagePath(*repository)
	var pullOptions *images.PullOptions
	if *publicBase {
		pullOptions = &images.PullOptions{}
	} else {
		pullOptions = &images.PullOptions{
			Username: username,
			Password: password,
		}
	}
	
	fmt.Println("Pulling test image...")
	if _, err := images.Pull(podmanContext, fullImagePath, pullOptions); err != nil {
		log.Errorf("Pull Error: %s", err)
		return false
	}

	return true
}

func imagePath(repository string) string {
	fullRepoPath := strings.Join([]string{repository, "latest"}, ":")
	return fullRepoPath
}

func fullImageRef(registry, repository, baseImage string) string {
	if baseImage != "" {
		imagePath := imagePath(baseImage) 
		fullImageRef := strings.Join([]string{registry, repository, imagePath}, "/")
		return fullImageRef
	} else {
		imagePath := imagePath(repository)
		fullImageRef := strings.Join([]string{registry, imagePath}, "/")
		return fullImageRef
	}
}

func pullBaseImage(podmanContext context.Context) bool {
	fullImagePath := fullImageRef(*registryHost, *repository, *baseImage)
	var pullOptions *images.PullOptions
	if *publicBase {
		pullOptions = &images.PullOptions{}
	} else {
		pullOptions = &images.PullOptions{
			Username: username,
			Password: password,
		}
	}

	if _, err := images.Pull(podmanContext, fullImagePath, pullOptions); err != nil {
		log.Errorf("Pull Error: %s", err)
		return false
	}

	return true
}

func deleteTopLayer(podmanContext context.Context) bool {
	var historyOptions *images.HistoryOptions
	imageHistory, err := images.History(podmanContext, *baseLayer, historyOptions)
	if err != nil {
		log.Errorf("%s", err)
		return false
	}

	for _, image := range imageHistory {
		if stringInSlice("latest", image.Tags) {
			log.Infof("Deleting image %s", image.ID)
			var imagesToRemove []string
			imagesToRemove[0] = image.ID
			_, err := images.Remove(podmanContext, imagesToRemove, &images.RemoveOptions{})
			if err != nil {
				log.Errorf("%s", err)
				return false
			}
			break
		}
	}

	return healthy
}

func createTagLayer(podmanContext context.Context) bool {
	container_name := fmt.Sprintf("updatedcontainer%v", time.Now().Unix())
	log.Infof("Creating new image via container %v", container_name)

	
	s := specgen.NewSpecGenerator(fullImageRef(*registryHost, *repository, *baseImage), false)
	s.Name = container_name
	createdResponse, err := containers.CreateWithSpec(podmanContext, s, nil)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("Container created.")

	var commitOptions *containers.CommitOptions
	if _, err := containers.Commit(podmanContext, createdResponse.ID, commitOptions); err != nil {
		log.Errorf("Error committing Container: %s", err)
		return false
	}

	if err := containers.Start(podmanContext, createdResponse.ID, nil); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("Container started.")

	log.Infof("Removing container: %s", createdResponse.ID)

	signal := "SIGKILL"
	var killOptions = &containers.KillOptions{Signal: &signal}
	if err := containers.Kill(podmanContext, createdResponse.ID, killOptions); err != nil {
		log.Errorf("Error removing container: %s", err)
		return false
	}

	return true
}

func pushTestImage(podmanContext context.Context) bool {
	pushOptions := &images.PushOptions{
		Username: username,
		Password: password,
	}

	source := fullImageRef(*registryHost, *repository, "")
	if err := images.Push(podmanContext, source, source, pushOptions); err != nil {
		log.Errorf("Push Error: %s", err)
		return false
	}

	return true
}

func init() {

	fmt.Println("init")
	var err error
	_, err = newPodmanClient()
	if err != nil {
		log.Fatalf("%s", err)
	}

}

func main() {
	// Parse the command line flags.
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		fmt.Println("1")
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	if err := flagutil.SetFlagsFromEnv(flag.CommandLine, "REGISTRY_MONITOR"); err != nil {
		fmt.Println("2")
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	lvl, err := log.ParseLevel(*level)
	if err != nil {
		fmt.Println("3")
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	log.SetLevel(lvl)

	// Ensure we have proper values.
	if *username == "" {
		log.Fatalln("Missing username flag")
	}

	if *password == "" {
		log.Fatalln("Missing password flag")
	}

	if *registryHost == "" {
		log.Fatalln("Missing registry-host flag")
	}

	if *repository == "" {
		log.Fatalln("Missing repository flag")
	}

	// TODO 
	if *baseImage == "" && *baseLayer == "" {
		log.Infoln("Missing base-image and base-layer-id flag; Dynamically assigning base-layer-id")
		grabID, err := images.History(podmanContext, *repository, &images.HistoryOptions{})
		// grabID, err := dockerClient.ImageHistory(*repository)
		if err != nil {
			log.Fatalf("Failed to grab image ID: %v", err)
		}
		log.Infof("Assigning base-layer-id to %s", grabID[0].ID)
		*baseLayer = grabID[0].ID
	} else if *baseImage != "" && *baseLayer != "" {
		log.Fatalln("Both base-image and base-layer-id flag; only one of required")
	}

	// Register the metrics.
	for _, metric := range prometheusMetrics {
		err := prometheus.Register(metric)
		if err != nil {
			log.Fatalf("Failed to register metric: %v", err)
		}
	}

	// Setup the HTTP server.
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/status", statusHandler)

	log.Infoln("Listening on", *listen)

	// Run the monitor routine.
	runMonitor()

	// Listen and serve.
	fmt.Println("listen and server")
	log.Fatal(http.ListenAndServe(*listen, nil))
}

func putCloudWatchMetric(metricName string, watchService *cloudwatch.CloudWatch, unitName string, metricValue float64) {
	if watchService == nil {
		return
	}

	params := &cloudwatch.PutMetricDataInput{
		MetricData: []*cloudwatch.MetricDatum{
			&cloudwatch.MetricDatum{
				MetricName: aws.String(metricName),
				Timestamp:  aws.Time(time.Now()),
				Unit:       aws.String(unitName),
				Value:      aws.Float64(metricValue),
			},
		},
		Namespace: aws.String(*cloudwatchNamespace),
	}

	_, err := watchService.PutMetricData(params)
	if err != nil {
		log.Printf("Failure to put cloudwatch metric: %s", err)
	}
	log.Printf("Reports to cloudwatch success")
}

func reportSuccess(watchService *cloudwatch.CloudWatch) {
	status = true
	m, err := promSuccessMetric.GetMetricWithLabelValues()
	if err != nil {
		panic(err)
	}
	m.Inc()
	putCloudWatchMetric(*cloudwatchSuccessMetric, watchService, "Count", 1)
}

func reportFailure(watchService *cloudwatch.CloudWatch) {
	status = false
	m, err := promFailureMetric.GetMetricWithLabelValues()
	if err != nil {
		panic(err)
	}
	m.Inc()
	putCloudWatchMetric(*cloudwatchFailureMetric, watchService, "Count", 1)
}

func reportPushTime(watchService *cloudwatch.CloudWatch, duration time.Duration) {
	promPushMetric.Observe(duration.Seconds())
	putCloudWatchMetric(*cloudwatchPushTimeMetric, watchService, "Seconds", duration.Seconds())
}

func reportPullTime(watchService *cloudwatch.CloudWatch, duration time.Duration) {
	promPullMetric.Observe(duration.Seconds())
	putCloudWatchMetric(*cloudwatchPullTimeMetric, watchService, "Seconds", duration.Seconds())
}

func runMonitor() {
	firstLoop := true
	healthy = true
	duration := 120 * time.Second
	mainLoop := func() {
		userDuration, err := time.ParseDuration(*testInterval)
		if err != nil {
			log.Fatalf("Failed to parse time interval: %v", err)
		}

		var cloudwatchService *cloudwatch.CloudWatch
		if *awsAccessKey != "" && *awsSecretKey != "" && *cloudwatchNamespace != "" {
			log.Infof("Configuring CloudWatch metrics reporting")
			aws_creds := credentials.NewStaticCredentials(*awsAccessKey, *awsSecretKey, "")
			sess, _ := session.NewSession(&aws.Config{Region: aws.String(*cloudwatchRegion), Credentials: aws_creds})
			cloudwatchService = cloudwatch.New(sess)
		}

		for {
			if !firstLoop {
				log.Infof("Sleeping for %v", duration)
				time.Sleep(duration)
			}

			log.Infof("Starting test")
			firstLoop = false
			status = true

			podmanContext, err = newPodmanClient()

			log.Infof("Pulling test image")
			pullStartTime := time.Now()
			if !pullTestImage(podmanContext) {
				duration = 30 * time.Second
				reportFailure(cloudwatchService)
				continue
			}

			// Write the pull time metric.
			reportPullTime(cloudwatchService, time.Since(pullStartTime))

			if *baseImage != "" {
				log.Infof("Pulling specified base image")
				if !pullBaseImage(podmanContext) {
					healthy = false
					return
				}

				base = *baseImage
			} else {
				base = *baseLayer
			}

			log.Infof("Deleting top layer")
			if !deleteTopLayer(podmanContext) {
				healthy = false
				return
			}

			log.Infof("Creating new top layer")
			if !createTagLayer(podmanContext) {
				healthy = false
				return
			}

			log.Infof("Pushing test image")
			pushStartTime := time.Now()
			if !pushTestImage(podmanContext) {
				duration = 30 * time.Second
				reportFailure(cloudwatchService)
				continue
			}

			// Write the push time metric.
			reportPushTime(cloudwatchService, time.Since(pushStartTime))

			log.Infof("Test successful")
			duration = userDuration

			// Write the success metric.
			reportSuccess(cloudwatchService)
		}
	}

	go mainLoop()
}
