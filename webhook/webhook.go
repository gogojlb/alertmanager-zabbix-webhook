package webhook

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	zabbix "github.com/blacked/go-zabbix"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

var log = logrus.WithField("context", "webhook")

type WebHook struct {
	channel chan *Event
	config  WebHookConfig
}

type WebHookConfig struct {
	Port          int          `yaml:"port"`
	QueueCapacity int          `yaml:"queueCapacity"`
	ZabbixConfig  ZabbixConfig `yaml:"zabbix"`
}

type ZabbixConfig struct {
	Host      string `yaml:"Host"`
	Port      int    `yaml:"Port"`
	HostNodes []int  `yaml:"HostNodes"`
}

type HookRequest struct {
	Trigger struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"trigger"`
	Events  []Event `json:"events"`
	Contact struct {
		Type  string `json:"type"`
		Value string `json:"value"`
		ID    string `json:"id"`
		User  string `json:"user"`
	} `json:"contact"`
	Plot      string `json:"plot"`
	Throttled bool   `json:"throttled"`
}
type Event struct {
	Metric       string `json:"metric"`
	Value        int    `json:"value"`
	Timestamp    int    `json:"timestamp"`
	TriggerEvent bool   `json:"trigger_event"`
	State        string `json:"state"`
	OldState     string `json:"old_state"`
}

// type HookRequest struct {
// 	Version           string            `json:"version"`
// 	GroupKey          string            `json:"groupKey"`
// 	Status            string            `json:"status"`
// 	Receiver          string            `json:"receiver"`
// 	GroupLabels       map[string]string `json:"groupLabels"`
// 	CommonLabels      map[string]string `json:"commonLabels"`
// 	CommonAnnotations map[string]string `json:"commonAnnotations"`
// 	ExternalURL       string            `json:"externalURL"`
// 	Alerts            []Alert           `json:"alerts"`
// }

// type Alert struct {
// 	Status       string            `json:"status"`
// 	Labels       map[string]string `json:"labels"`
// 	Annotations  map[string]string `json:"annotations"`
// 	StartsAt     string            `json:"startsAt,omitempty"`
// 	EndsAt       string            `json:"endsAt,omitempty"`
// 	GeneratorURL string            `json:"generatorURL"`
// }

func New(cfg *WebHookConfig) *WebHook {
	return &WebHook{
		channel: make(chan *Event, cfg.QueueCapacity),
		config:  *cfg,
	}
}

func ConfigFromFile(filename string) (cfg *WebHookConfig, err error) {
	log.Infof("Loading configuration at '%s'", filename)
	configFile, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("can't open the config file: %s", err)
	}

	// Default values
	config := WebHookConfig{
		Port:          8080,
		QueueCapacity: 500,
		ZabbixConfig: ZabbixConfig{
			Host:      "127.0.0.1",
			Port:      10051,
			HostNodes: []int{0, 1},
		},
	}

	err = yaml.Unmarshal(configFile, &config)
	if err != nil {
		return nil, fmt.Errorf("can't read the config file: %s", err)
	}

	log.Info("Configuration loaded")
	return &config, nil
}

func (hook *WebHook) Start() error {

	// Launch the process thread
	go hook.processEvents()

	// Launch the listening thread
	log.Println("Initializing HTTP server")
	http.HandleFunc("/alerts", hook.alertsHandler)
	err := http.ListenAndServe(":"+strconv.Itoa(hook.config.Port), nil)
	if err != nil {
		return fmt.Errorf("can't start the listening thread: %s", err)
	}

	log.Info("Exiting")
	close(hook.channel)

	return nil
}

func (hook *WebHook) alertsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		hook.postHandler(w, r)
	default:
		http.Error(w, "unsupported HTTP method", 400)
	}
}

func (hook *WebHook) postHandler(w http.ResponseWriter, r *http.Request) {

	dec := json.NewDecoder(r.Body)
	defer r.Body.Close()

	var m HookRequest
	if err := dec.Decode(&m); err != nil {
		log.Errorf("error decoding message: %v", err)
		http.Error(w, "request body is not valid json", 400)
		return
	}
	var alertEvents []Event
	for eventIndex := range m.Events {
		if m.Events[eventIndex].TriggerEvent == true {
			alertEvents = append(alertEvents, m.Events[eventIndex])
		}
	}
	for index := range alertEvents {
		hook.channel <- &alertEvents[index]
	}

}

func (hook *WebHook) processEvents() {

	log.Info("Alerts queue started")

	// While there are alerts in the queue, batch them and send them over to Zabbix
	var zabbixMetrics []*zabbix.Metric
	for {
		select {
		case e := <-hook.channel:
			if e == nil {
				log.Info("Queue Closed")
				return
			}
			var moiraMetrics []string
			var hostNodes []string
			moiraMetrics = strings.Split(e.Metric, ".")

			for _, moiraHostNode := range hook.config.ZabbixConfig.HostNodes {
				hostNodes = append(hostNodes, moiraMetrics[moiraHostNode])
			}
			host := strings.Join(hostNodes, " ")

			// host, exists := a.Annotations[hook.config.ZabbixHostAnnotation]
			// if !exists {
			// 	host = hook.config.ZabbixHostDefault
			// }

			// Send alerts only if a host annotation is present or configuration for default host is not empty
			if host != "" {
				key := fmt.Sprintf("%s", e.Metric)
				value := "0"
				if e.State == "ERROR" {
					value = "2"
				}
				if e.State == "WARN" {
					value = "1"
				}

				log.Infof("added Zabbix metrics, host: '%s' key: '%s', value: '%s'", host, key, value)
				zabbixMetrics = append(zabbixMetrics, zabbix.NewMetric(host, key, value))
			}
		default:
			if len(zabbixMetrics) != 0 {
				hook.zabbixSend(zabbixMetrics)
				zabbixMetrics = zabbixMetrics[:0]
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}
}

func (hook *WebHook) zabbixSend(metrics []*zabbix.Metric) {
	// Create instance of Packet class
	packet := zabbix.NewPacket(metrics)

	// Send packet to zabbix
	log.Infof("sending to zabbix '%s:%d'", hook.config.ZabbixConfig.Host, hook.config.ZabbixConfig.Port)
	z := zabbix.NewSender(hook.config.ZabbixConfig.Host, hook.config.ZabbixConfig.Port)
	_, err := z.Send(packet)
	if err != nil {
		log.Error(err)
	} else {
		log.Info("successfully sent")
	}

}
