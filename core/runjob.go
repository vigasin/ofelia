package core

import (
	"fmt"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gobs/args"
	"strings"
	"os"
	"path"
		"io/ioutil"
	"encoding/json"
	"os/exec"
	"log"
	"io"
)

var dockercfg *docker.AuthConfigurations

func dockerAuth(repository string) (docker.AuthConfiguration, error){
	configPath := path.Join(os.Getenv("HOME"), ".docker", "config.json")
	configFile, err := os.Open(configPath)

	if err != nil {
		return docker.AuthConfiguration{}, err
	}

	byteData, _ := ioutil.ReadAll(configFile)

	confsWrapper := struct {
		CredsStore string `json:"credsStore"`
	}{}
	if err := json.Unmarshal(byteData, &confsWrapper); err != nil {
		log.Println("Can't read credsStore")
		return docker.AuthConfiguration{}, err
	}

	command := fmt.Sprintf("docker-credential-%s", confsWrapper.CredsStore)

	cmd := exec.Command(command, "get")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		defer stdin.Close()
		io.WriteString(stdin, repository)
	}()

	out, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}

	authWrapper := struct {
		ServerURL string
		Username string
		Secret string
	}{}

	if err := json.Unmarshal(out, &authWrapper); err != nil {
		log.Println("Can't read auth")
		return docker.AuthConfiguration{}, err
	}

	result := docker.AuthConfiguration{
		ServerAddress:fmt.Sprintf("https://%s", authWrapper.ServerURL),
		Username:authWrapper.Username,
		Password:authWrapper.Secret,
		Email:"no@email.no",
	}

	return result, nil
}

func init() {
	dockercfg, _ = docker.NewAuthConfigurationsFromDockerCfg()
}

type RunJob struct {
	BareJob   `mapstructure:",squash"`
	Client    *docker.Client `json:"-"`
	User      string         `default:"root"`
	TTY       bool           `default:"false"`
	Delete    bool           `default:"true"`
	Pull      bool           `default:"true"`
	Image     string
	Network   string
	Container string
	Volumes   string
}

func NewRunJob(c *docker.Client) *RunJob {
	return &RunJob{Client: c}
}

func (j *RunJob) Run(ctx *Context) error {
	var container *docker.Container
	var err error
	if j.Image != "" && j.Container == "" {
		if err = func() error {
			var err error

			// if Pull option "true"
			// try pulling image first
			if j.Pull {
				if err = j.pullImage(); err == nil {
					ctx.Log("Pulled image " + j.Image)
					return nil
				}
			}

			// if Pull option "false"
			// try to find image locally
			if err = j.searchLocalImage(); err == nil {
				ctx.Log("Found locally image " + j.Image)
				return nil
			}

			if !j.Pull && err == ErrLocalImageNotFound {
				// if couldn't find locally, try to pull
				if err = j.pullImage(); err == nil {
					ctx.Log("Pulled image " + j.Image)
					return nil
				}
			}

			return err
		}(); err != nil {
			return err
		}

		container, err = j.buildContainer()
		if err != nil {
			return err
		}
	} else {
		container, err = j.getContainer(j.Container)
		if err != nil {
			return err
		}
	}

	if err := j.startContainer(ctx.Execution, container); err != nil {
		return err
	}

	if err := j.watchContainer(container.ID); err != nil {
		return err
	}

	if j.Container == "" {
		return j.deleteContainer(container.ID)
	}
	return nil
}

func (j *RunJob) searchLocalImage() error {
	imgs, err := j.Client.ListImages(buildFindLocalImageOptions(j.Image))
	if err != nil {
		return err
	}

	if len(imgs) != 1 {
		return ErrLocalImageNotFound
	}

	return nil
}

func (j *RunJob) pullImage() error {
	o, a := buildPullOptions(j.Image)
	if err := j.Client.PullImage(o, a); err != nil {
		return fmt.Errorf("error pulling image %q: %s", j.Image, err)
	}

	return nil
}

func (j *RunJob) buildContainer() (*docker.Container, error) {
	volumeList := strings.Split(j.Volumes, ";")

	var mounts []docker.HostMount

	for _, volume := range volumeList {
		if volume == "" {
			continue
		}

		volumeSplit := strings.Split(volume, ":")

		mount := docker.HostMount{
			Type: "bind",
			Source: volumeSplit[0],
			Target:volumeSplit[1],
		}
		mounts = append(mounts, mount)
	}

	c, err := j.Client.CreateContainer(docker.CreateContainerOptions{
		Config: &docker.Config{
			Image:        j.Image,
			AttachStdin:  false,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          j.TTY,
			Cmd:          args.GetArgs(j.Command),
			User:         j.User,
		},
		NetworkingConfig: &docker.NetworkingConfig{},
		HostConfig:&docker.HostConfig{
			Mounts: mounts,
		},
	})

	if err != nil {
		return c, fmt.Errorf("error creating exec: %s", err)
	}

	if j.Network != "" {
		networkOpts := docker.NetworkFilterOpts{}
		networkOpts["name"] = map[string]bool{}
		networkOpts["name"][j.Network] = true
		if networks, err := j.Client.FilteredListNetworks(networkOpts); err == nil {
			for _, network := range networks {
				if err := j.Client.ConnectNetwork(network.ID, docker.NetworkConnectionOptions{
					Container: c.ID,
				}); err != nil {
					return c, fmt.Errorf("error connecting container to network: %s", err)
				}
			}
		}
	}

	return c, nil
}

func (j *RunJob) startContainer(e *Execution, c *docker.Container) error {
	err := j.Client.StartContainer(c.ID, &docker.HostConfig{})
	if err != nil {
		return err
	}

	_, err = j.Client.AttachToContainerNonBlocking(docker.AttachToContainerOptions{
		Container: c.ID,
		Logs: true,
		Stdout: true,
		Stderr: true,
		OutputStream: e.OutputStream,
		ErrorStream: e.ErrorStream,
	})

	return err
}

func (j *RunJob) getContainer(id string) (*docker.Container, error) {
	container, err := j.Client.InspectContainer(id)
	if err != nil {
		return nil, err
	}
	return container, nil
}

const (
	watchDuration      = time.Millisecond * 100
	maxProcessDuration = time.Hour * 24
)

func (j *RunJob) watchContainer(containerID string) error {
	var s docker.State
	var r time.Duration
	for {
		time.Sleep(watchDuration)
		r += watchDuration

		if r > maxProcessDuration {
			return ErrMaxTimeRunning
		}

		c, err := j.Client.InspectContainer(containerID)
		if err != nil {
			return err
		}

		if !c.State.Running {
			s = c.State
			break
		}
	}

	switch s.ExitCode {
	case 0:
		return nil
	case -1:
		return ErrUnexpected
	default:
		return fmt.Errorf("error non-zero exit code: %d", s.ExitCode)
	}
}

func (j *RunJob) deleteContainer(containerID string) error {
	if !j.Delete {
		return nil
	}

	return j.Client.RemoveContainer(docker.RemoveContainerOptions{
		ID: containerID,
	})
}
