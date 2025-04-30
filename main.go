package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	upstreamDnsDefault    = "8.8.8.8:53,4.4.4.4:53"
	listenDnsPortDefault  = "53"
	listenHttpPortDisable = "-1"

	ingressMap = make(map[string]string)
	gatewayMap = make(map[string]string)

	namespaceFlag        *string = nil
	kubeconfigFlag       *string = nil
	upstreamsFlag        *string = nil
	listenDnsPortFlag    *string = nil
	listenHttpPortFlag   *string = nil
	debugFlag            *bool   = nil
	enableGatewayApiFlag *bool   = nil

	upstreamsList []string
	log           *zap.Logger

	mapMutateLock sync.Mutex

	dnsUdpClient *dns.Client
	dnsTcpClient *dns.Client
)

func init() {
	dnsUdpClient = &dns.Client{}
	dnsTcpClient = &dns.Client{Net: "tcp"}

	debugFlag = flag.Bool("debug", false, "Enable debug logging")
	if home := homedir.HomeDir(); home != "" {
		d := filepath.Join(home, ".kube", "config")
		kubeconfigFlag = flag.String("kubeconfig", d, "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfigFlag = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	namespaceFlag = flag.String("namespace", "", "(optional) watch ingress objects only in the namespace")
	upstreamsFlag = flag.String("upstreams", upstreamDnsDefault, "(optional) coma-separated [type://]host:port list of upstreams")
	listenDnsPortFlag = flag.String("dns-port", listenDnsPortDefault, "(optional) dns interface port")
	listenHttpPortFlag = flag.String("http-port", listenHttpPortDisable, "(optional) http interface port, disabled by default")
	enableGatewayApiFlag = flag.Bool("enable-gateway-api", false, "(optional) enable Gateway API support")
	flag.Parse()

	if *debugFlag {
		log = zap.Must(zap.NewDevelopment())
		log.Info("Starting development logger")
	} else {
		log = zap.Must(zap.NewProduction())
		log.Info("Starting production logger")
	}

	upstreamsList = strings.Split(*upstreamsFlag, ",")
	for i, k := range upstreamsList {
		upstreamsList[i] = strings.TrimSpace(k)
	}
	log.Info("Loaded upstreams", zap.String("upstreams", strings.Join(upstreamsList, ",")))
}

func main() {
	stopChan := make(chan struct{})
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	if err := startIngressInformer(stopChan); err != nil {
		log.Error("Failed to start ingress informer, exiting...", zap.Error(err))
		os.Exit(1)
	}
	if err := startGatewayInformer(stopChan); err != nil {
		log.Error("Failed to start Gateway informer, exiting...", zap.Error(err))
		os.Exit(1)
	}
	if err := startHttpRouteInformer(stopChan); err != nil {
		log.Error("Failed to start HttpRoute informer, exiting...", zap.Error(err))
		os.Exit(1)
	}
	if err := startDnsServer(stopChan); err != nil {
		log.Error("Failed to start DNS server, exiting...", zap.Error(err))
		os.Exit(1)
	}
	if err := startHttpServer(stopChan); err != nil {
		log.Error("Failed to start HTTP server, exiting...", zap.Error(err))
		os.Exit(1)
	}

	<-exitChan
	log.Info("Shutting down...")
	log.Sync()
	close(stopChan)
}

func lock() {
	mapMutateLock.Lock()
}

func unlock() {
	mapMutateLock.Unlock()
}

func startHttpServer(quitChan <-chan struct{}) error {
	if *listenHttpPortFlag == listenHttpPortDisable {
		log.Debug("Port is disabled, skipping the HTTP server...")
		return nil
	}

	http.HandleFunc("/", handleHttpRequest)
	srv := http.Server{
		Addr: fmt.Sprintf(":%s", *listenHttpPortFlag),
	}
	go func() {
		log.Info("Starting HTTP server...", zap.String("port", *listenHttpPortFlag))
		if err := srv.ListenAndServe(); err != nil {
			log.Fatal("Failed to start HTTP server", zap.Error(err))
		}
	}()
	go func() {
		<-quitChan
		srv.Shutdown(context.Background())
	}()
	return nil
}

func startDnsServer(quitChan <-chan struct{}) error {
	dns.HandleFunc(".", handleDnsRequest)
	srv := &dns.Server{
		Addr: fmt.Sprintf(":%s", *listenDnsPortFlag),
		Net:  "udp",
	}
	go func() {
		log.Info("Starting DNS server...", zap.String("port", *listenDnsPortFlag))
		if err := srv.ListenAndServe(); err != nil {
			log.Fatal("Failed to start DNS server", zap.Error(err))
		}
	}()
	go func() {
		<-quitChan
		srv.Shutdown()
	}()
	return nil
}

func startIngressInformer(quitChan <-chan struct{}) error {
	client, err := getStaticClient()
	if err != nil {
		return fmt.Errorf("failed to get kubernetes client")
	}

	var opts []informers.SharedInformerOption
	if *namespaceFlag != "" {
		opts = append(opts, informers.WithNamespace(*namespaceFlag))
	}

	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, opts...)
	informer := factory.Networking().V1().Ingresses().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			updateIngressResource(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			updateIngressResource(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			deleteIngressResource(obj)
		},
	})

	go func() {
		log.Info("Starting Ingress informer...")
		informer.Run(quitChan)
	}()

	if !cache.WaitForCacheSync(quitChan, informer.HasSynced) {
		return fmt.Errorf("failed to sync ingress informer cache")
	}
	return nil
}

func startGatewayInformer(quitChan <-chan struct{}) error {
	if !*enableGatewayApiFlag {
		log.Debug("Gateway API disabled, skipping the Gateway informer...")
		return nil
	}

	client, err := getDynamicClient()
	if err != nil {
		log.Error("Failed to create dynamic client", zap.Error(err))
		return err
	}
	namespace := metav1.NamespaceAll
	if *namespaceFlag != "" {
		namespace = *namespaceFlag
	}

	gvr := schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, namespace, nil)
	informer := factory.ForResource(gvr).Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			updateGatewayResource(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			updateGatewayResource(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			deleteGatewayResource(obj)
		},
	})

	go func() {
		log.Info("Starting Gateway informer...")
		informer.Run(quitChan)
	}()
	if !cache.WaitForCacheSync(quitChan, informer.HasSynced) {
		return fmt.Errorf("failed to sync gateway informer cache")
	}
	return nil
}

func startHttpRouteInformer(quitChan <-chan struct{}) error {
	if !*enableGatewayApiFlag {
		log.Debug("Gateway API disabled, skipping the HttpRoute informer...")
		return nil
	}

	client, err := getDynamicClient()
	if err != nil {
		log.Error("Failed to create dynamic client", zap.Error(err))
		return err
	}
	namespace := metav1.NamespaceAll
	if *namespaceFlag != "" {
		namespace = *namespaceFlag
	}
	gvr := schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "httproutes", // note: plural name of the CRD
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, namespace, nil)
	informer := factory.ForResource(gvr).Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			updateHttpRouteResource(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			updateHttpRouteResource(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			deleteHttpRouteResource(obj)
		},
	})

	go func() {
		log.Info("Starting HttpRoute informer...")
		informer.Run(quitChan)
	}()
	if !cache.WaitForCacheSync(quitChan, informer.HasSynced) {
		return fmt.Errorf("failed to sync httproute informer cache")
	}
	return nil
}

func getConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Info("In-cluster config not found, trying kubeconfig...",
			zap.String("kubeconfig", *kubeconfigFlag),
			zap.Error(err),
		)
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfigFlag)
		if err != nil {
			log.Error("Failed load Kubernetes config", zap.Error(err))
			return nil, err
		}
	}
	return config, err
}

func getStaticClient() (*kubernetes.Clientset, error) {
	config, err := getConfig()
	if err != nil {
		log.Error("Error loading Kubernetes config", zap.Error(err))
		return nil, err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Error("Error creating Kubernetes client", zap.Error(err))
		return nil, err
	}
	return client, nil
}

func getDynamicClient() (*dynamic.DynamicClient, error) {
	config, err := getConfig()
	if err != nil {
		log.Error("Error loading Kubernetes config", zap.Error(err))
		return nil, err
	}

	client, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Error("Error creating Kubernetes client", zap.Error(err))
		return nil, err
	}
	return client, nil
}

func formatGatewayKey(name, namespace, kind string) string {
	return strings.ToLower(fmt.Sprintf("name:%s,namespace:%s,kind:%s", name, namespace, kind))
}

func deleteGatewayResource(obj interface{}) {
	i, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Debug("Failed to cast to Unstructured, skipping...")
		return
	}
	key := formatGatewayKey(i.GetName(), i.GetNamespace(), i.GetKind())
	delete(gatewayMap, key)

	log.Info("Gateway deleted",
		zap.String("name", i.GetName()),
		zap.String("namespace", i.GetNamespace()),
	)
}

func updateGatewayResource(obj interface{}) {
	i, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Debug("Failed to cast to Unstructured, skipping...")
		return
	}

	addressesSlice, found, err := unstructured.NestedSlice(i.Object, "status", "addresses")
	if err != nil {
		log.Error("Error reading status.addresses",
			zap.Error(err),
			zap.String("name", i.GetName()),
			zap.String("namespace", i.GetNamespace()),
		)
		return
	}
	if !found {
		log.Error("No addresses found for Gateway",
			zap.String("name", i.GetName()),
			zap.String("namespace", i.GetNamespace()),
		)
		return
	}

	ip := ""
	for _, addr := range addressesSlice {
		a, ok := addr.(map[string]interface{})
		if !ok {
			log.Warn("Unable to convert addressesSlice into map[string]interface{}",
				zap.String("name", i.GetName()),
				zap.String("namespace", i.GetNamespace()),
			)
		}

		typ := a["type"]
		val := a["value"]
		if typ == "IPAddress" {
			ip = val.(string)
			break
		}
	}

	if ip == "" {
		log.Warn("No IPAddress found",
			zap.String("name", i.GetName()),
			zap.String("namespace", i.GetNamespace()),
		)
		return
	}

	key := formatGatewayKey(i.GetName(), i.GetNamespace(), i.GetKind())
	gatewayMap[key] = ip

	log.Info("Gateway discovered",
		zap.String("name", i.GetName()),
		zap.String("namespace", i.GetNamespace()),
		zap.String("key", key),
		zap.String("ip", ip),
	)
}

func getHttpRouteHostnamesAndGateways(i *unstructured.Unstructured) ([]string, []string, error) {
	hostnames, found, err := unstructured.NestedStringSlice(i.Object, "spec", "hostnames")
	if err != nil {
		log.Error("Error reading spec.hostnames",
			zap.Error(err),
			zap.String("name", i.GetName()),
			zap.String("namespace", i.GetNamespace()),
		)
		return nil, nil, fmt.Errorf("error reading spec.hostnames")
	}
	if !found {
		log.Error("No hostnames found for HttpRoute",
			zap.String("name", i.GetName()),
			zap.String("namespace", i.GetNamespace()),
		)
		return nil, nil, fmt.Errorf("no hostnames found for HttpRoute")
	}
	parentRefsSlice, found, err := unstructured.NestedSlice(i.Object, "spec", "parentRefs")
	if err != nil {
		log.Error("Error reading spec.parentRefs",
			zap.Error(err),
			zap.String("name", i.GetName()),
			zap.String("namespace", i.GetNamespace()),
		)
		return hostnames, nil, fmt.Errorf("error reading spec.parentRefs")
	}
	if !found {
		log.Error("no parentRefs found for HttpRoute",
			zap.String("name", i.GetName()),
			zap.String("namespace", i.GetNamespace()),
		)
		return hostnames, nil, fmt.Errorf("No parentRefs found for HttpRoute")
	}

	var gatewayKeys []string
	for _, p := range parentRefsSlice {
		v, ok := p.(map[string]interface{})
		if !ok {
			log.Warn("Unable to convert parentRefSlice into map[string]string",
				zap.String("name", i.GetName()),
				zap.String("namespace", i.GetNamespace()),
			)
		}

		gwName := v["name"].(string)
		gwNamespace := v["namespace"].(string)
		gwKind := v["kind"].(string)

		if gwKind != "Gateway" {
			log.Warn("Unsupported parentRef kind, skipping...",
				zap.String("httproute-name", i.GetName()),
				zap.String("httproute-namespace", i.GetNamespace()),
				zap.String("kind", gwKind),
				zap.String("gateway-name", gwName),
				zap.String("gateway-namespace", gwNamespace),
			)
			continue
		}

		key := formatGatewayKey(gwName, gwNamespace, gwKind)
		gatewayKeys = append(gatewayKeys, key)
	}
	return hostnames, gatewayKeys, nil
}

func deleteHttpRouteResource(obj interface{}) {
	i, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Debug("Failed to cast to Unstructured, skipping...")
		return
	}
	hostnames, gateways, err := getHttpRouteHostnamesAndGateways(i)
	if err != nil {
		log.Error("Failed to parse HttpRoute object")
		return
	}

	lock()
	for _, host := range hostnames {
		delete(ingressMap, host)
	}
	unlock()

	log.Info("HttpRoute deleted",
		zap.String("name", i.GetName()),
		zap.String("namespace", i.GetNamespace()),
		zap.Strings("hostnames", hostnames),
		zap.Strings("gateways", gateways),
	)
}

func updateHttpRouteResource(obj interface{}) {
	i, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Debug("Failed to cast to Unstructured, skipping...")
		return
	}
	hostnames, gateways, err := getHttpRouteHostnamesAndGateways(i)
	if err != nil {
		log.Error("Failed to parse HttpRoute object")
		return
	}

	for _, key := range gateways {
		gatewayIpAddress, ok := gatewayMap[key]
		if !ok {
			log.Warn("Gateway spec not found in cache", zap.String("key", key))
			continue
		}

		lock()
		for _, host := range hostnames {
			ingressMap[host] = gatewayIpAddress
		}
		unlock()

		log.Info("HttpRoute discovered",
			zap.Strings("hostnames", hostnames),
			zap.Strings("gateways", gateways),
		)
	}
}

func deleteIngressResource(obj interface{}) {
	i, ok := obj.(*networkingv1.Ingress)
	if !ok {
		log.Debug("failed to cast to Ingress, skipping...")
		return
	}
	if len(i.Status.LoadBalancer.Ingress) == 0 {
		log.Debug("no LoadBalancer.Ingress, skipping...", zap.String("name", i.Name))
		return
	}

	lock()
	for _, rule := range i.Spec.Rules {
		delete(ingressMap, rule.Host)
	}
	unlock()

	log.Info("Ingress deleted",
		zap.String("name", i.Name),
		zap.String("namespace", i.Namespace),
	)
}

func updateIngressResource(obj interface{}) {
	i, ok := obj.(*networkingv1.Ingress)
	if !ok {
		log.Debug("failed to cast to Ingress, skipping...")
		return
	}
	if len(i.Status.LoadBalancer.Ingress) == 0 {
		log.Debug("no LoadBalancer.Ingress, skipping...", zap.String("name", i.Name))
		return
	}

	lock()
	ip := i.Status.LoadBalancer.Ingress[0].IP
	for _, rule := range i.Spec.Rules {
		ingressMap[rule.Host] = ip
		log.Info("Ingress rule discovered",
			zap.String("host", rule.Host),
			zap.String("ip", ip),
		)
	}
	unlock()
}

func handleHttpRequest(w http.ResponseWriter, r *http.Request) {
	log.Debug("HTTP request", zap.String("uri", r.RequestURI))
	w.WriteHeader(http.StatusNoContent)
	w.Write([]byte{})
}

func handleDnsRequest(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, q := range r.Question {
		log.Debug("Question",
			zap.String("name", q.Name),
			zap.String("type", dns.TypeToString[q.Qtype]),
		)

		if q.Qtype == dns.TypeA {
			host := strings.TrimSuffix(q.Name, ".")
			if ip, ok := ingressMap[host]; ok {
				a := &dns.A{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    300,
					},
					A: net.ParseIP(ip),
				}
				msg.Answer = append(msg.Answer, a)
				w.WriteMsg(msg)
				return
			}
		}

		if q.Qtype == dns.TypeAAAA {
			host := strings.TrimSuffix(q.Name, ".")
			if _, ok := ingressMap[host]; ok {
				msg.Answer = nil
				w.WriteMsg(msg)
				return
			}
		}
	}

	log.Debug("Not an Ingress host, forwarding request", zap.String("host", r.Question[0].Name))
	forwardDnsRequest(w, r)
}

func forwardDnsRequest(w dns.ResponseWriter, r *dns.Msg) {
	for _, u := range upstreamsList {
		var resp *dns.Msg
		var err error
		var rrt time.Duration

		log.Debug("Calling upstream",
			zap.String("upstream", u),
			zap.String("question", r.Question[0].String()),
		)

		switch true {
		case strings.HasPrefix(u, "udp://") || strings.Index(u, "://") <= 0:
			h := strings.TrimPrefix(strings.TrimPrefix(u, "udp://"), "://")
			resp, rrt, err = dnsUdpClient.Exchange(r, h)
		case strings.HasPrefix(u, "tcp://"):
			h := strings.TrimPrefix(u, "tcp://")
			resp, rrt, err = dnsTcpClient.Exchange(r, h)
		default:
			log.Error("Unknown upstream format",
				zap.String("question", r.Question[0].String()),
				zap.String("upstream", u),
			)
			err = fmt.Errorf("unable to parse upstream: '%s'", u)
			resp = nil
		}

		q := strings.Replace(r.Question[0].String(), "\t", " ", -1)
		if err != nil {
			log.Warn("Upstream exchange failure",
				zap.String("upstream", u),
				zap.String("question", q),
				zap.Duration("rrt", rrt),
				zap.Error(err),
			)
			continue
		}
		if len(resp.Answer) > 0 {
			a := strings.Replace(resp.Answer[0].String(), "\t", " ", -1)
			log.Debug("Upstream response received",
				zap.String("upstream", u),
				zap.String("question", q),
				zap.String("answer", a),
			)
		} else {
			log.Debug("Upstream response is empty",
				zap.String("upstream", u),
				zap.String("question", q),
			)
		}
		w.WriteMsg(resp)
		return
	}

	log.Warn("All upstreams are dead, failing the request")
	m := &dns.Msg{}
	m.SetReply(r).SetRcode(r, dns.RcodeServerFailure)
	w.WriteMsg(m)
}
