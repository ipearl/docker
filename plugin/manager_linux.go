// +build linux

package plugin

import (
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/libcontainerd"
	"github.com/docker/docker/oci"
	"github.com/docker/docker/pkg/plugins"
	"github.com/docker/docker/plugin/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func (pm *Manager) enable(p *v2.Plugin, c *controller, force bool) error {
	if p.IsEnabled() && !force {
		return fmt.Errorf("plugin %s is already enabled", p.Name())
	}
	spec, err := p.InitSpec(oci.DefaultSpec(), pm.libRoot)
	if err != nil {
		return err
	}

	c.restart = true
	c.exitChan = make(chan bool)

	pm.mu.Lock()
	pm.cMap[p] = c
	pm.mu.Unlock()

	if err := pm.containerdClient.Create(p.GetID(), "", "", specs.Spec(*spec), attachToLog(p.GetID())); err != nil {
		return err
	}

	client, err := plugins.NewClientWithTimeout("unix://"+filepath.Join(p.GetRuntimeSourcePath(), p.GetSocket()), nil, c.timeoutInSecs)
	if err != nil {
		c.restart = false
		shutdownPlugin(p, c, pm.containerdClient)
		return err
	}

	p.SetPClient(client)
	pm.pluginStore.SetState(p, true)
	pm.pluginStore.CallHandler(p)

	return nil
}

func (pm *Manager) restore(p *v2.Plugin) error {
	return pm.containerdClient.Restore(p.GetID(), attachToLog(p.GetID()))
}

func shutdownPlugin(p *v2.Plugin, c *controller, containerdClient libcontainerd.Client) {
	pluginID := p.GetID()

	err := containerdClient.Signal(pluginID, int(syscall.SIGTERM))
	if err != nil {
		logrus.Errorf("Sending SIGTERM to plugin failed with error: %v", err)
	} else {
		select {
		case <-c.exitChan:
			logrus.Debug("Clean shutdown of plugin")
		case <-time.After(time.Second * 10):
			logrus.Debug("Force shutdown plugin")
			if err := containerdClient.Signal(pluginID, int(syscall.SIGKILL)); err != nil {
				logrus.Errorf("Sending SIGKILL to plugin failed with error: %v", err)
			}
		}
	}
}

func (pm *Manager) disable(p *v2.Plugin, c *controller) error {
	if !p.IsEnabled() {
		return fmt.Errorf("plugin %s is already disabled", p.Name())
	}

	c.restart = false
	shutdownPlugin(p, c, pm.containerdClient)
	pm.pluginStore.SetState(p, false)
	return nil
}

// Shutdown stops all plugins and called during daemon shutdown.
func (pm *Manager) Shutdown() {
	plugins := pm.pluginStore.GetAll()
	for _, p := range plugins {
		pm.mu.RLock()
		c := pm.cMap[p]
		pm.mu.RUnlock()

		if pm.liveRestore && p.IsEnabled() {
			logrus.Debug("Plugin active when liveRestore is set, skipping shutdown")
			continue
		}
		if pm.containerdClient != nil && p.IsEnabled() {
			c.restart = false
			shutdownPlugin(p, c, pm.containerdClient)
		}
	}
}
