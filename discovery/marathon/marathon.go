// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package marathon

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/util/httputil"
)

const (
	// metaLabelPrefix is the meta prefix used for all meta labels in this discovery.
	metaLabelPrefix = model.MetaLabelPrefix + "marathon_"
	// appLabelPrefix is the prefix for the application labels.
	appLabelPrefix = metaLabelPrefix + "app_label_"

	// appLabel is used for the name of the app in Marathon.
	appLabel model.LabelName = metaLabelPrefix + "app"
	// imageLabel is the label that is used for the docker image running the service.
	imageLabel model.LabelName = metaLabelPrefix + "image"
	// taskLabel contains the mesos task name of the app instance.
	taskLabel model.LabelName = metaLabelPrefix + "task"

	// Constants for instrumentation.
	namespace = "prometheus"
)

var (
	refreshFailuresCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "sd_marathon_refresh_failures_total",
			Help:      "The number of Marathon-SD refresh failures.",
		})
	refreshDuration = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace: namespace,
			Name:      "sd_marathon_refresh_duration_seconds",
			Help:      "The duration of a Marathon-SD refresh in seconds.",
		})
)

func init() {
	prometheus.MustRegister(refreshFailuresCount)
	prometheus.MustRegister(refreshDuration)
}

const appListPath string = "/v2/apps/?embed=apps.tasks"

// Discovery provides service discovery based on a Marathon instance.
type Discovery struct {
	client          *http.Client
	servers         []string
	refreshInterval time.Duration
	lastRefresh     map[string]*config.TargetGroup
	appsClient      AppListClient
}

// Initialize sets up the discovery for usage.
func NewDiscovery(conf *config.MarathonSDConfig) (*Discovery, error) {
	tls, err := httputil.NewTLSConfig(conf.TLSConfig)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout: time.Duration(conf.Timeout),
		Transport: &http.Transport{
			TLSClientConfig: tls,
		},
	}

	return &Discovery{
		client:          client,
		servers:         conf.Servers,
		refreshInterval: time.Duration(conf.RefreshInterval),
		appsClient:      fetchApps,
	}, nil
}

// Run implements the TargetProvider interface.
func (md *Discovery) Run(ctx context.Context, ch chan<- []*config.TargetGroup) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(md.refreshInterval):
			err := md.updateServices(ctx, ch)
			if err != nil {
				log.Errorf("Error while updating services: %s", err)
			}
		}
	}
}

func (md *Discovery) updateServices(ctx context.Context, ch chan<- []*config.TargetGroup) (err error) {
	t0 := time.Now()
	defer func() {
		refreshDuration.Observe(time.Since(t0).Seconds())
		if err != nil {
			refreshFailuresCount.Inc()
		}
	}()

	targetMap, err := md.fetchTargetGroups()
	if err != nil {
		return err
	}

	all := make([]*config.TargetGroup, 0, len(targetMap))
	for _, tg := range targetMap {
		all = append(all, tg)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- all:
	}

	// Remove services which did disappear.
	for source := range md.lastRefresh {
		_, ok := targetMap[source]
		if !ok {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- []*config.TargetGroup{{Source: source}}:
				log.Debugf("Removing group for %s", source)
			}
		}
	}

	md.lastRefresh = targetMap
	return nil
}

func (md *Discovery) fetchTargetGroups() (map[string]*config.TargetGroup, error) {
	url := RandomAppsURL(md.servers)
	apps, err := md.appsClient(md.client, url)
	if err != nil {
		return nil, err
	}

	groups := AppsToTargetGroups(apps)
	return groups, nil
}

// Task describes one instance of a service running on Marathon.
type Task struct {
	ID    string   `json:"id"`
	Host  string   `json:"host"`
	Ports []uint32 `json:"ports"`
}

// DockerContainer describes a container which uses the docker runtime.
type DockerContainer struct {
	Image string `json:"image"`
}

// Container describes the runtime an app in running in.
type Container struct {
	Docker DockerContainer `json:"docker"`
}

// App describes a service running on Marathon.
type App struct {
	ID           string            `json:"id"`
	Tasks        []Task            `json:"tasks"`
	RunningTasks int               `json:"tasksRunning"`
	Labels       map[string]string `json:"labels"`
	Container    Container         `json:"container"`
}

// AppList is a list of Marathon apps.
type AppList struct {
	Apps []App `json:"apps"`
}

// AppListClient defines a function that can be used to get an application list from marathon.
type AppListClient func(client *http.Client, url string) (*AppList, error)

// fetchApps requests a list of applications from a marathon server.
func fetchApps(client *http.Client, url string) (*AppList, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return parseAppJSON(body)
}

func parseAppJSON(body []byte) (*AppList, error) {
	apps := &AppList{}
	err := json.Unmarshal(body, apps)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// RandomAppsURL randomly selects a server from an array and creates
// an URL pointing to the app list.
func RandomAppsURL(servers []string) string {
	// TODO: If possible update server list from Marathon at some point.
	server := servers[rand.Intn(len(servers))]
	return fmt.Sprintf("%s%s", server, appListPath)
}

// AppsToTargetGroups takes an array of Marathon apps and converts them into target groups.
func AppsToTargetGroups(apps *AppList) map[string]*config.TargetGroup {
	tgroups := map[string]*config.TargetGroup{}
	for _, a := range apps.Apps {
		group := createTargetGroup(&a)
		tgroups[group.Source] = group
	}
	return tgroups
}

func createTargetGroup(app *App) *config.TargetGroup {
	var (
		targets = targetsForApp(app)
		appName = model.LabelValue(app.ID)
		image   = model.LabelValue(app.Container.Docker.Image)
	)
	tg := &config.TargetGroup{
		Targets: targets,
		Labels: model.LabelSet{
			appLabel:   appName,
			imageLabel: image,
		},
		Source: app.ID,
	}

	for ln, lv := range app.Labels {
		ln = appLabelPrefix + ln
		tg.Labels[model.LabelName(ln)] = model.LabelValue(lv)
	}

	return tg
}

func targetsForApp(app *App) []model.LabelSet {
	targets := make([]model.LabelSet, 0, len(app.Tasks))
	for _, t := range app.Tasks {
		if len(t.Ports) == 0 {
			continue
		}
		target := targetForTask(&t)
		targets = append(targets, model.LabelSet{
			model.AddressLabel: model.LabelValue(target),
			taskLabel:          model.LabelValue(t.ID),
		})
	}
	return targets
}

func targetForTask(task *Task) string {
	return net.JoinHostPort(task.Host, fmt.Sprintf("%d", task.Ports[0]))
}
