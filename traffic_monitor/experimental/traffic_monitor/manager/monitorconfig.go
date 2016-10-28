package manager

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/common/log"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/common/poller"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/config"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/enum"
	"github.com/apache/incubator-trafficcontrol/traffic_monitor/experimental/traffic_monitor/peer"
	to "github.com/apache/incubator-trafficcontrol/traffic_ops/client"
)

// CopyTrafficMonitorConfigMap returns a deep copy of the given TrafficMonitorConfigMap
func CopyTrafficMonitorConfigMap(a *to.TrafficMonitorConfigMap) to.TrafficMonitorConfigMap {
	b := to.TrafficMonitorConfigMap{}
	b.TrafficServer = map[string]to.TrafficServer{}
	b.CacheGroup = map[string]to.TMCacheGroup{}
	b.Config = map[string]interface{}{}
	b.TrafficMonitor = map[string]to.TrafficMonitor{}
	b.DeliveryService = map[string]to.TMDeliveryService{}
	b.Profile = map[string]to.TMProfile{}
	for k, v := range a.TrafficServer {
		b.TrafficServer[k] = v
	}
	for k, v := range a.CacheGroup {
		b.CacheGroup[k] = v
	}
	for k, v := range a.Config {
		b.Config[k] = v
	}
	for k, v := range a.TrafficMonitor {
		b.TrafficMonitor[k] = v
	}
	for k, v := range a.DeliveryService {
		b.DeliveryService[k] = v
	}
	for k, v := range a.Profile {
		b.Profile[k] = v
	}
	return b
}

// TrafficMonitorConfigMapThreadsafe encapsulates a TrafficMonitorConfigMap safe for multiple readers and a single writer.
type TrafficMonitorConfigMapThreadsafe struct {
	monitorConfig *to.TrafficMonitorConfigMap
	m             *sync.RWMutex
}

// NewTrafficMonitorConfigMapThreadsafe returns an encapsulated TrafficMonitorConfigMap safe for multiple readers and a single writer.
func NewTrafficMonitorConfigMapThreadsafe() TrafficMonitorConfigMapThreadsafe {
	return TrafficMonitorConfigMapThreadsafe{monitorConfig: &to.TrafficMonitorConfigMap{}, m: &sync.RWMutex{}}
}

// Get returns the TrafficMonitorConfigMap. Callers MUST NOT modify, it is not threadsafe for mutation. If mutation is necessary, call CopyTrafficMonitorConfigMap().
func (t *TrafficMonitorConfigMapThreadsafe) Get() to.TrafficMonitorConfigMap {
	t.m.RLock()
	defer t.m.RUnlock()
	return *t.monitorConfig
}

// Set sets the TrafficMonitorConfigMap. This is only safe for one writer. This MUST NOT be called by multiple threads.
func (t *TrafficMonitorConfigMapThreadsafe) Set(c to.TrafficMonitorConfigMap) {
	t.m.Lock()
	*t.monitorConfig = c
	t.m.Unlock()
}

// StartMonitorConfigManager runs the monitor config manager goroutine, and returns the threadsafe data which it sets.
func StartMonitorConfigManager(
	monitorConfigPollChan <-chan to.TrafficMonitorConfigMap,
	localStates peer.CRStatesThreadsafe,
	statURLSubscriber chan<- poller.HttpPollerConfig,
	healthURLSubscriber chan<- poller.HttpPollerConfig,
	peerURLSubscriber chan<- poller.HttpPollerConfig,
	cachesChangeSubscriber chan<- struct{},
	cfg config.Config,
	staticAppData StaticAppData,
) TrafficMonitorConfigMapThreadsafe {
	monitorConfig := NewTrafficMonitorConfigMapThreadsafe()
	go monitorConfigListen(monitorConfig,
		monitorConfigPollChan,
		localStates,
		statURLSubscriber,
		healthURLSubscriber,
		peerURLSubscriber,
		cachesChangeSubscriber,
		cfg,
		staticAppData,
	)
	return monitorConfig
}

// trafficOpsHealthConnectionTimeoutToDuration takes the int from Traffic Ops, which is in milliseconds, and returns a time.Duration
// TODO change Traffic Ops Client API to a time.Duration
func trafficOpsHealthConnectionTimeoutToDuration(t int) time.Duration {
	return time.Duration(t) * time.Millisecond
}

// trafficOpsPeerPollIntervalToDuration takes the int from Traffic Ops, which is in milliseconds, and returns a time.Duration
// TODO change Traffic Ops Client API to a time.Duration
func trafficOpsPeerPollIntervalToDuration(t int) time.Duration {
	return time.Duration(t) * time.Millisecond
}

// trafficOpsStatPollIntervalToDuration takes the int from Traffic Ops, which is in milliseconds, and returns a time.Duration
// TODO change Traffic Ops Client API to a time.Duration
func trafficOpsStatPollIntervalToDuration(t int) time.Duration {
	return time.Duration(t) * time.Millisecond
}

// trafficOpsHealthPollIntervalToDuration takes the int from Traffic Ops, which is in milliseconds, and returns a time.Duration
// TODO change Traffic Ops Client API to a time.Duration
func trafficOpsHealthPollIntervalToDuration(t int) time.Duration {
	return time.Duration(t) * time.Millisecond
}

// getPollIntervals reads the Traffic Ops Client monitorConfig structure, and parses and returns the health, peer, and stat poll intervals
func getHealthPeerStatPollIntervals(monitorConfig to.TrafficMonitorConfigMap) (time.Duration, time.Duration, time.Duration, error) {
	healthPollIntervalI, healthPollIntervalExists := monitorConfig.Config["health.polling.interval"]
	if !healthPollIntervalExists {
		return 0, 0, 0, fmt.Errorf("Traffic Ops Monitor config missing 'health.polling.interval', not setting config changes.\n")
	}
	healthPollIntervalInt, healthPollIntervalIsInt := healthPollIntervalI.(float64)
	if !healthPollIntervalIsInt {
		return 0, 0, 0, fmt.Errorf("Traffic Ops Monitor config 'health.polling.interval' value '%v' type %T is not an integer, not setting config changes.\n", healthPollIntervalI, healthPollIntervalI)
	}
	healthPollInterval := trafficOpsHealthPollIntervalToDuration(int(healthPollIntervalInt))

	peerPollIntervalI, peerPollIntervalExists := monitorConfig.Config["peers.polling.interval"]
	if !peerPollIntervalExists {
		return 0, 0, 0, fmt.Errorf("Traffic Ops Monitor config missing 'peers.polling.interval', not setting config changes.\n")
	}
	peerPollIntervalInt, peerPollIntervalIsInt := peerPollIntervalI.(float64)
	if !peerPollIntervalIsInt {
		return 0, 0, 0, fmt.Errorf("Traffic Ops Monitor config 'peers.polling.interval' value '%v' type %T is not an integer, not setting config changes.\n", peerPollIntervalI, peerPollIntervalI)
	}
	peerPollInterval := trafficOpsHealthPollIntervalToDuration(int(peerPollIntervalInt))

	statPollIntervalI, statPollIntervalExists := monitorConfig.Config["stat.polling.interval"]
	if !statPollIntervalExists {
		log.Warnf("Traffic Ops Monitor config missing 'stat.polling.interval', using health for stat.\n")
		statPollIntervalI = healthPollIntervalI
	}
	statPollIntervalInt, statPollIntervalIsInt := statPollIntervalI.(float64)
	if !statPollIntervalIsInt {
		log.Warnf("Traffic Ops Monitor config 'stat.polling.interval' value '%v' type %T is not an integer, using health for stat\n", statPollIntervalI, statPollIntervalI)
		statPollIntervalI = healthPollIntervalI
	}
	statPollInterval := trafficOpsHealthPollIntervalToDuration(int(statPollIntervalInt))

	// Formerly, only 'health' polling existed. If TO still has old configuration and doesn't have a 'stat' parameter, this allows us to assume the 'health' poll is slow, and sets it to the stat poll (which used to be the only poll, getting all astats data) to the given presumed-slow health poll, and set the now-fast-and-small health poll to a short fraction of that.
	// TODO make config?
	healthIsQuarterStatIfStatNotExist := true
	if healthIsQuarterStatIfStatNotExist {
		if healthPollIntervalExists && !statPollIntervalExists {
			healthPollInterval = healthPollInterval / 4
		}
	}

	return healthPollInterval, peerPollInterval, statPollInterval, nil
}

// TODO timing, and determine if the case, or its internal `for`, should be put in a goroutine
// TODO determine if subscribers take action on change, and change to mutexed objects if not.
func monitorConfigListen(
	monitorConfigTS TrafficMonitorConfigMapThreadsafe,
	monitorConfigPollChan <-chan to.TrafficMonitorConfigMap,
	localStates peer.CRStatesThreadsafe,
	statURLSubscriber chan<- poller.HttpPollerConfig,
	healthURLSubscriber chan<- poller.HttpPollerConfig,
	peerURLSubscriber chan<- poller.HttpPollerConfig,
	cachesChangeSubscriber chan<- struct{},
	cfg config.Config,
	staticAppData StaticAppData,
) {
	for monitorConfig := range monitorConfigPollChan {
		monitorConfigTS.Set(monitorConfig)
		healthUrls := map[string]poller.PollConfig{}
		statUrls := map[string]poller.PollConfig{}
		peerUrls := map[string]poller.PollConfig{}
		caches := map[string]string{}

		healthPollInterval, peerPollInterval, statPollInterval, err := getHealthPeerStatPollIntervals(monitorConfig)
		if err != nil {
			continue
		}

		for _, srv := range monitorConfig.TrafficServer {
			caches[srv.HostName] = srv.Status

			cacheName := enum.CacheName(srv.HostName)

			if srv.Status == "ONLINE" {
				localStates.SetCache(cacheName, peer.IsAvailable{IsAvailable: true})
				continue
			}
			if srv.Status == "OFFLINE" {
				continue
			}
			// seed states with available = false until our polling cycle picks up a result
			if _, exists := localStates.Get().Caches[cacheName]; !exists {
				localStates.SetCache(cacheName, peer.IsAvailable{IsAvailable: false})
			}

			url := monitorConfig.Profile[srv.Profile].Parameters.HealthPollingURL
			r := strings.NewReplacer(
				"${hostname}", srv.IP,
				"${interface_name}", srv.InterfaceName,
				"application=system", "application=plugin.remap",
				"application=", "application=plugin.remap",
			)
			url = r.Replace(url)

			connTimeout := trafficOpsHealthConnectionTimeoutToDuration(monitorConfig.Profile[srv.Profile].Parameters.HealthConnectionTimeout)
			healthUrls[srv.HostName] = poller.PollConfig{URL: url, Timeout: connTimeout}
			r = strings.NewReplacer("application=plugin.remap", "application=")
			statUrl := r.Replace(url)
			statUrls[srv.HostName] = poller.PollConfig{URL: statUrl, Timeout: connTimeout}
		}

		for _, srv := range monitorConfig.TrafficMonitor {
			if srv.HostName == staticAppData.Hostname {
				continue
			}
			if srv.Status != "ONLINE" {
				continue
			}
			// TODO: the URL should be config driven. -jse
			url := fmt.Sprintf("http://%s:%d/publish/CrStates?raw", srv.IP, srv.Port)
			peerUrls[srv.HostName] = poller.PollConfig{URL: url} // TODO determine timeout.
		}

		statURLSubscriber <- poller.HttpPollerConfig{Urls: statUrls, Interval: statPollInterval}
		healthURLSubscriber <- poller.HttpPollerConfig{Urls: healthUrls, Interval: healthPollInterval}
		peerURLSubscriber <- poller.HttpPollerConfig{Urls: peerUrls, Interval: peerPollInterval}

		for cacheName := range localStates.GetCaches() {
			if _, exists := monitorConfig.TrafficServer[string(cacheName)]; !exists {
				log.Warnf("Removing %s from localStates", cacheName)
				localStates.DeleteCache(cacheName)
			}
		}

		cachesChangeSubscriber <- struct{}{}

		// TODO because there are multiple writers to localStates.DeliveryService, there is a race condition, where MonitorConfig (this func) and HealthResultManager could write at the same time, and the HealthResultManager could overwrite a delivery service addition or deletion here. Probably the simplest and most performant fix would be a lock-free algorithm using atomic compare-and-swaps.
		for _, ds := range monitorConfig.DeliveryService {
			// since caches default to unavailable, also default DS false
			if _, exists := localStates.Get().Deliveryservice[enum.DeliveryServiceName(ds.XMLID)]; !exists {
				localStates.SetDeliveryService(enum.DeliveryServiceName(ds.XMLID), peer.Deliveryservice{IsAvailable: false, DisabledLocations: []enum.CacheName{}}) // important to initialize DisabledLocations, so JSON is `[]` not `null`
			}
		}
		for ds := range localStates.Get().Deliveryservice {
			if _, exists := monitorConfig.DeliveryService[string(ds)]; !exists {
				localStates.DeleteDeliveryService(ds)
			}
		}
	}
}
