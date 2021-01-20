package cli

import (
	docker "github.com/fsouza/go-dockerclient"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vigasin/ofelia/core"
)

// DaemonCommand daemon process
type DaemonCommand struct {
	ConfigFile         string `long:"config" description:"configuration file" default:"/etc/ofelia.conf"`
	DockerLabelsConfig bool   `short:"d" long:"docker" description:"read configurations from docker labels"`

	config    *Config
	scheduler *core.Scheduler
	signals   chan os.Signal
	done      chan bool
	events    chan *docker.APIEvents
}

// Execute runs the daemon
func (c *DaemonCommand) Execute(args []string) error {
	_, err := os.Stat("/.dockerenv")
	IsDockerEnv = !os.IsNotExist(err)

	exit := false

	for {
		if err := c.boot(); err != nil {
			return err
		}

		if err := c.start(); err != nil {
			return err
		}

		exit, err = c.shutdown()
		if err != nil {
			return err
		}

		if exit {
			break
		} else {
			time.Sleep(1 * time.Second)
		}
	}

	return nil
}

func (c *DaemonCommand) boot() (err error) {
	if c.DockerLabelsConfig {
		c.scheduler, err = BuildFromDockerLabels()
		c.setWaiter()
		if err != nil {
			return err
		}
	} else {
		c.scheduler, err = BuildFromFile(c.ConfigFile)
	}

	return
}

func (c *DaemonCommand) start() error {
	c.setSignals()

	if err := c.scheduler.Start(); err != nil {
		return err
	}

	return nil
}

func (c *DaemonCommand) setSignals() {
	c.signals = make(chan os.Signal, 1)
	c.done = make(chan bool, 1)

	signal.Notify(c.signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-c.signals
		c.scheduler.Logger.Warningf(
			"Signal received: %s, shutting down the process\n", sig,
		)

		c.done <- true
	}()
}

func (c *DaemonCommand) setWaiter() error {
	c.events = make(chan *docker.APIEvents, 10)

	d, err := docker.NewClientFromEnv()
	if err != nil {
		return err
	}

	return d.AddEventListener(c.events)
}

func (c *DaemonCommand) unsetWaiter() error {
	d, err := docker.NewClientFromEnv()
	if err != nil {
		return err
	}

	return d.RemoveEventListener(c.events)
}

func (c *DaemonCommand) shutdown() (bool, error) {
	var needExit bool
	var needBreak bool

	for {
		select {
		case <-c.done:
			needExit = true
			needBreak = true
		case event := <-c.events:
			needExit = false

			if event.Type == "container" && (event.Action == "die" || event.Action == "start") {
				// We'll reset it soon
				c.unsetWaiter()
				needBreak = true
			} else {
				needBreak = false
			}
		}

		if needBreak {
			break
		}
	}

	if !c.scheduler.IsRunning() {
		return needExit, nil
	}

	c.scheduler.Logger.Warningf("Waiting running jobs.")
	return needExit, c.scheduler.Stop()
}
