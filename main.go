package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	upstreamDnsDefault = "8.8.8.8:53,4.4.4.4:53"
	listenPortDefault  = "53"

	ingressMap    = make(map[string]string)
	upstreamsList []string

	namespaceFlag  *string = nil
	kubeconfigFlag *string = nil
	upstreamsFlag  *string = nil
	listenPortFlag *string = nil
	debugFlag      *bool   = nil

	log *zap.Logger = nil
)

func init() {
	debugFlag = flag.Bool("debug", false, "Enable debug logging")
	if home := homedir.HomeDir(); home != "" {
		d := filepath.Join(home, ".kube", "config")
		kubeconfigFlag = flag.String("kubeconfig", d, "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfigFlag = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	namespaceFlag = flag.String("namespace", "", "(optional) watch ingress objects only in the namespace")
	upstreamsFlag = flag.String("upstreams", upstreamDnsDefault, "(optional) coma-separated host:port list of upstreams")
	listenPortFlag = flag.String("port", listenPortDefault, "(optional) listen port")
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

	startIngressInformer(stopChan)
	startDnsServer(stopChan)

	<-exitChan
	log.Info("Shutting down...")
	log.Sync()
	close(stopChan)
}

func startDnsServer(quitChan <-chan struct{}) error {
	dns.HandleFunc(".", handleDnsRequest)
	srv := &dns.Server{
		Addr: fmt.Sprintf(":%s", *listenPortFlag),
		Net:  "udp",
	}
	go func() {
		log.Info("Starting DNS server...", zap.String("port", *listenPortFlag))
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
	clientset, err := getClientset()
	if err != nil {
		return err
	}

	var opts []informers.SharedInformerOption
	if *namespaceFlag != "" {
		opts = append(opts, informers.WithNamespace(*namespaceFlag))
	}

	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0, opts...)
	informer := factory.Networking().V1().Ingresses().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			updateIngressMap(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			updateIngressMap(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			deleteIngressMap(obj)
		},
	})

	go func() {
		log.Info("Starting Ingress informer...")
		informer.Run(quitChan)
	}()

	if !cache.WaitForCacheSync(quitChan, informer.HasSynced) {
		log.Error("Failed to sync informer cache")
		return err
	}
	return nil
}

func getClientset() (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	config, err = rest.InClusterConfig()
	if err != nil {
		log.Info("In-cluster config failed", zap.Error(err))
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfigFlag)
		if err != nil {
			log.Error("Failed load Kubernetes config", zap.Error(err))
			return nil, err
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Error("Error creating Kubernetes client", zap.Error(err))
		return nil, err
	}
	return clientset, nil
}

func deleteIngressMap(obj interface{}) {
	i, ok := obj.(*networkingv1.Ingress)
	if !ok {
		log.Debug("failed to cast to Ingress, skipping...")
		return
	}
	if len(i.Status.LoadBalancer.Ingress) == 0 {
		log.Debug("no LoadBalancer.Ingress, skipping...", zap.String("name", i.Name))
		return
	}
	for _, rule := range i.Spec.Rules {
		delete(ingressMap, rule.Host)
		log.Debug("Cleared up host",
			zap.String("host", rule.Host),
		)
	}
	log.Debug("Ingress deleted", zap.String("name", i.Name))
}

func updateIngressMap(obj interface{}) {
	i, ok := obj.(*networkingv1.Ingress)
	if !ok {
		log.Debug("failed to cast to Ingress, skipping...")
		return
	}
	if len(i.Status.LoadBalancer.Ingress) == 0 {
		log.Debug("no LoadBalancer.Ingress, skipping...", zap.String("name", i.Name))
		return
	}

	ip := i.Status.LoadBalancer.Ingress[0].IP
	for _, rule := range i.Spec.Rules {
		ingressMap[rule.Host] = ip
		log.Debug("Host discovered",
			zap.String("host", rule.Host),
			zap.String("ip", ip),
		)
	}
	log.Info("Ingress loaded",
		zap.String("name", i.Name),
		zap.String("namespace", i.Namespace),
		zap.String("load balancer", ip),
	)
}

func handleDnsRequest(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, q := range r.Question {
		log.Debug("New request",
			zap.String("type", dns.TypeToString[q.Qtype]),
			zap.String("name", q.Name),
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

	log.Debug("Forwarging the request...")
	forwardDnsRequest(w, r)
}

func forwardDnsRequest(w dns.ResponseWriter, req *dns.Msg) {
	c := new(dns.Client)

	for _, u := range upstreamsList {
		log.Debug("Communicating to upstream", zap.String("upstream", u))
		resp, _, err := c.Exchange(req, u)
		if err != nil {
			log.Warn("Failed to get response from upstream, skipping...",
				zap.String("upstream", u),
				zap.Error(err),
			)
			continue
		}
		w.WriteMsg(resp)
		return
	}

	log.Warn("No more upstreams, faling request")
	dns.HandleFailed(w, req)
}
