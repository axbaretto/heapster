// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:generate ./hooks/run_extpoints.sh

package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"

	"k8s.io/heapster/common/flags"
	kube_config "k8s.io/heapster/common/kubernetes"
	"k8s.io/heapster/metrics/cmd/heapster-apiserver/app"
	"k8s.io/heapster/metrics/core"
	"k8s.io/heapster/metrics/manager"
	"k8s.io/heapster/metrics/options"
	"k8s.io/heapster/metrics/processors"
	"k8s.io/heapster/metrics/sinks"
	"k8s.io/heapster/metrics/sinks/metric"
	"k8s.io/heapster/metrics/sources"
	"k8s.io/heapster/version"
	kube_api "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/cache"
	kube_client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/healthz"
	"k8s.io/kubernetes/pkg/util/flag"
	"k8s.io/kubernetes/pkg/util/logs"
	"k8s.io/kubernetes/pkg/version/verflag"
)

func main() {
	opt := options.NewHeapsterRunOptions()
	opt.AddFlags(pflag.CommandLine)

	flag.InitFlags()
	logs.InitLogs()
	defer logs.FlushLogs()
	verflag.PrintAndExitIfRequested()

	setMaxProcs(opt)
	glog.Infof(strings.Join(os.Args, " "))
	glog.Infof("Heapster version %v", version.HeapsterVersion)
	if err := validateFlags(opt); err != nil {
		glog.Fatal(err)
	}

	kubernetesUrl, err := getKubernetesAddress(opt.Sources)
	if err != nil {
		glog.Fatalf("Failed to get kubernetes address: %v", err)
	}
	sourceManager := createSourceManagerOrDie(opt.Sources)
	sinkManager, metricSink, historicalSource := createAndInitSinksOrDie(opt.Sinks, opt.HistoricalSource)

	podLister, nodeLister := getListersOrDie(kubernetesUrl)
	dataProcessors := createDataProcessorsOrDie(kubernetesUrl, podLister)

	man, err := manager.NewManager(sourceManager, dataProcessors, sinkManager,
		opt.MetricResolution, manager.DefaultScrapeOffset, manager.DefaultMaxParallelism)
	if err != nil {
		glog.Fatalf("Failed to create main manager: %v", err)
	}
	man.Start()

	if opt.EnableAPIServer {
		// Run API server in a separate goroutine
		createAndRunAPIServer(opt, metricSink, nodeLister, podLister)
	}

	mux := http.NewServeMux()
	promHandler := prometheus.Handler()
	handler := setupHandlers(metricSink, podLister, nodeLister, historicalSource)
	healthz.InstallHandler(mux, healthzChecker(metricSink))

	addr := fmt.Sprintf("%s:%d", opt.Ip, opt.Port)
	glog.Infof("Starting heapster on port %d", opt.Port)

	if len(opt.TLSCertFile) > 0 && len(opt.TLSKeyFile) > 0 {
		startSecureServing(opt, handler, promHandler, mux, addr)
	} else {
		mux.Handle("/", handler)
		mux.Handle("/metrics", promHandler)

		glog.Fatal(http.ListenAndServe(addr, mux))
	}
}
func createAndRunAPIServer(opt *options.HeapsterRunOptions, metricSink *metricsink.MetricSink,
	nodeLister *cache.StoreToNodeLister, podLister *cache.StoreToPodLister) {

	server, err := app.NewHeapsterApiServer(opt, metricSink, nodeLister, podLister)
	if err != nil {
		// TODO: Should this be fatal?
		glog.Errorf("Could not create the API server: %v", err)
		return
	}
	healthz.InstallHandler(server.MuxHelper, healthzChecker(metricSink))
	runApiServer := func(s *app.HeapsterAPIServer) {
		if err := s.RunServer(); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}
	glog.Infof("Starting Heapster API server on port %d", opt.InsecurePort)
	go runApiServer(server)
}

func startSecureServing(opt *options.HeapsterRunOptions, handler http.Handler, promHandler http.Handler,
	mux *http.ServeMux, address string) {

	if len(opt.TLSClientCAFile) > 0 {
		authPprofHandler, err := newAuthHandler(opt, handler)
		if err != nil {
			glog.Fatalf("Failed to create authorized pprof handler: %v", err)
		}
		handler = authPprofHandler

		authPromHandler, err := newAuthHandler(opt, promHandler)
		if err != nil {
			glog.Fatalf("Failed to create authorized prometheus handler: %v", err)
		}
		promHandler = authPromHandler
	}
	mux.Handle("/", handler)
	mux.Handle("/metrics", promHandler)

	// If allowed users is set, then we need to enable Client Authentication
	if len(opt.AllowedUsers) > 0 {
		server := &http.Server{
			Addr:      address,
			Handler:   mux,
			TLSConfig: &tls.Config{ClientAuth: tls.RequestClientCert},
		}
		glog.Fatal(server.ListenAndServeTLS(opt.TLSCertFile, opt.TLSKeyFile))
	} else {
		glog.Fatal(http.ListenAndServeTLS(address, opt.TLSCertFile, opt.TLSKeyFile, mux))
	}
}

func createSourceManagerOrDie(src flags.Uris) core.MetricsSource {
	if len(src) != 1 {
		glog.Fatal("Wrong number of sources specified")
	}
	sourceFactory := sources.NewSourceFactory()
	sourceProvider, err := sourceFactory.BuildAll(src)
	if err != nil {
		glog.Fatalf("Failed to create source provide: %v", err)
	}
	sourceManager, err := sources.NewSourceManager(sourceProvider, sources.DefaultMetricsScrapeTimeout)
	if err != nil {
		glog.Fatalf("Failed to create source manager: %v", err)
	}
	return sourceManager
}

func createAndInitSinksOrDie(sinkAddresses flags.Uris, historicalSource string) (core.DataSink, *metricsink.MetricSink, core.HistoricalSource) {
	sinksFactory := sinks.NewSinkFactory()
	metricSink, sinkList, histSource := sinksFactory.BuildAll(sinkAddresses, historicalSource)
	if metricSink == nil {
		glog.Fatal("Failed to create metric sink")
	}
	if histSource == nil && len(historicalSource) > 0 {
		glog.Fatal("Failed to use a sink as a historical metrics source")
	}
	for _, sink := range sinkList {
		glog.Infof("Starting with %s", sink.Name())
	}
	sinkManager, err := sinks.NewDataSinkManager(sinkList, sinks.DefaultSinkExportDataTimeout, sinks.DefaultSinkStopTimeout)
	if err != nil {
		glog.Fatalf("Failed to created sink manager: %v", err)
	}
	return sinkManager, metricSink, histSource
}

func getListersOrDie(kubernetesUrl *url.URL) (*cache.StoreToPodLister, *cache.StoreToNodeLister) {
	kubeClient := createKubeClientOrDie(kubernetesUrl)

	podLister, err := getPodLister(kubeClient)
	if err != nil {
		glog.Fatalf("Failed to create podLister: %v", err)
	}
	nodeLister, err := getNodeLister(kubeClient)
	if err != nil {
		glog.Fatalf("Failed to create nodeLister: %v", err)
	}
	return podLister, nodeLister
}

func createKubeClientOrDie(kubernetesUrl *url.URL) *kube_client.Client {
	kubeConfig, err := kube_config.GetKubeClientConfig(kubernetesUrl)
	if err != nil {
		glog.Fatalf("Failed to get client config: %v", err)
	}
	return kube_client.NewOrDie(kubeConfig)
}

func createDataProcessorsOrDie(kubernetesUrl *url.URL, podLister *cache.StoreToPodLister) []core.DataProcessor {
	dataProcessors := []core.DataProcessor{
		// Convert cumulaties to rate
		processors.NewRateCalculator(core.RateMetricsMapping),
	}

	podBasedEnricher, err := processors.NewPodBasedEnricher(podLister)
	if err != nil {
		glog.Fatalf("Failed to create PodBasedEnricher: %v", err)
	}
	dataProcessors = append(dataProcessors, podBasedEnricher)

	namespaceBasedEnricher, err := processors.NewNamespaceBasedEnricher(kubernetesUrl)
	if err != nil {
		glog.Fatalf("Failed to create NamespaceBasedEnricher: %v", err)
	}
	dataProcessors = append(dataProcessors, namespaceBasedEnricher)

	// aggregators
	metricsToAggregate := []string{
		core.MetricCpuUsageRate.Name,
		core.MetricMemoryUsage.Name,
		core.MetricCpuRequest.Name,
		core.MetricCpuLimit.Name,
		core.MetricMemoryRequest.Name,
		core.MetricMemoryLimit.Name,
	}

	metricsToAggregateForNode := []string{
		core.MetricCpuRequest.Name,
		core.MetricCpuLimit.Name,
		core.MetricMemoryRequest.Name,
		core.MetricMemoryLimit.Name,
	}

	dataProcessors = append(dataProcessors,
		processors.NewPodAggregator(),
		&processors.NamespaceAggregator{
			MetricsToAggregate: metricsToAggregate,
		},
		&processors.NodeAggregator{
			MetricsToAggregate: metricsToAggregateForNode,
		},
		&processors.ClusterAggregator{
			MetricsToAggregate: metricsToAggregate,
		})

	nodeAutoscalingEnricher, err := processors.NewNodeAutoscalingEnricher(kubernetesUrl)
	if err != nil {
		glog.Fatalf("Failed to create NodeAutoscalingEnricher: %v", err)
	}
	dataProcessors = append(dataProcessors, nodeAutoscalingEnricher)
	return dataProcessors
}

const (
	minMetricsCount = 1
	maxMetricsDelay = 3 * time.Minute
)

func healthzChecker(metricSink *metricsink.MetricSink) healthz.HealthzChecker {
	return healthz.NamedCheck("healthz", func(r *http.Request) error {
		batch := metricSink.GetLatestDataBatch()
		if batch == nil {
			return errors.New("could not get the latest data batch")
		}
		if time.Since(batch.Timestamp) > maxMetricsDelay {
			message := fmt.Sprintf("No current data batch available (latest: %s).", batch.Timestamp.String())
			glog.Warningf(message)
			return errors.New(message)
		}
		if len(batch.MetricSets) < minMetricsCount {
			message := fmt.Sprintf("Not enough metrics found in the latest data batch: %d (expected min. %d) %s", len(batch.MetricSets), minMetricsCount, batch.Timestamp.String())
			glog.Warningf(message)
			return errors.New(message)
		}
		return nil
	})
}

// Gets the address of the kubernetes source from the list of source URIs.
// Possible kubernetes sources are: 'kubernetes' and 'kubernetes.summary_api'
func getKubernetesAddress(args flags.Uris) (*url.URL, error) {
	for _, uri := range args {
		if strings.SplitN(uri.Key, ".", 2)[0] == "kubernetes" {
			return &uri.Val, nil
		}
	}
	return nil, fmt.Errorf("No kubernetes source found.")
}

func getPodLister(kubeClient *kube_client.Client) (*cache.StoreToPodLister, error) {
	lw := cache.NewListWatchFromClient(kubeClient, "pods", kube_api.NamespaceAll, fields.Everything())
	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	podLister := &cache.StoreToPodLister{Indexer: store}
	reflector := cache.NewReflector(lw, &kube_api.Pod{}, store, time.Hour)
	reflector.Run()
	return podLister, nil
}

func getNodeLister(kubeClient *kube_client.Client) (*cache.StoreToNodeLister, error) {
	lw := cache.NewListWatchFromClient(kubeClient, "nodes", kube_api.NamespaceAll, fields.Everything())
	nodeLister := &cache.StoreToNodeLister{Store: cache.NewStore(cache.MetaNamespaceKeyFunc)}
	reflector := cache.NewReflector(lw, &kube_api.Node{}, nodeLister.Store, time.Hour)
	reflector.Run()
	return nodeLister, nil
}

func validateFlags(opt *options.HeapsterRunOptions) error {
	if opt.MetricResolution < 5*time.Second {
		return fmt.Errorf("metric resolution needs to be greater than 5 seconds - %d", opt.MetricResolution)
	}
	if (len(opt.TLSCertFile) > 0 && len(opt.TLSKeyFile) == 0) || (len(opt.TLSCertFile) == 0 && len(opt.TLSKeyFile) > 0) {
		return fmt.Errorf("both TLS certificate & key are required to enable TLS serving")
	}
	if len(opt.TLSClientCAFile) > 0 && len(opt.TLSCertFile) == 0 {
		return fmt.Errorf("client cert authentication requires TLS certificate & key")
	}
	return nil
}

func setMaxProcs(opt *options.HeapsterRunOptions) {
	// Allow as many threads as we have cores unless the user specified a value.
	var numProcs int
	if opt.MaxProcs < 1 {
		numProcs = runtime.NumCPU()
	} else {
		numProcs = opt.MaxProcs
	}
	runtime.GOMAXPROCS(numProcs)

	// Check if the setting was successful.
	actualNumProcs := runtime.GOMAXPROCS(0)
	if actualNumProcs != numProcs {
		glog.Warningf("Specified max procs of %d but using %d", numProcs, actualNumProcs)
	}
}
