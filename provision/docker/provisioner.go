// Copyright 2013 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/globocom/config"
	"github.com/globocom/tsuru/db"
	"github.com/globocom/tsuru/exec"
	"github.com/globocom/tsuru/log"
	"github.com/globocom/tsuru/provision"
	"github.com/globocom/tsuru/router"
	_ "github.com/globocom/tsuru/router/hipache"
	_ "github.com/globocom/tsuru/router/nginx"
	_ "github.com/globocom/tsuru/router/testing"
	"io"
	"labix.org/v2/mgo"
	"net"
	"strings"
	"sync"
)

func init() {
	provision.Register("docker", &DockerProvisioner{})
}

var (
	execut exec.Executor
	emutex sync.Mutex
)

func executor() exec.Executor {
	emutex.Lock()
	defer emutex.Unlock()
	if execut == nil {
		execut = exec.OsExecutor{}
	}
	return execut
}

func getRouter() (router.Router, error) {
	r, err := config.GetString("docker:router")
	if err != nil {
		return nil, err
	}
	return router.Get(r)
}

type DockerProvisioner struct{}

// Provision creates a container and install its dependencies
func (p *DockerProvisioner) Provision(app provision.App) error {
	return nil
}

func (p *DockerProvisioner) Restart(app provision.App) error {
	containers, err := getContainers(app.GetName())
	if err != nil {
		log.Printf("Got error while getting app containers: %s", err)
		return err
	}
	for _, c := range containers {
		err = c.stop()
		if err != nil {
			log.Printf("Error while stopping container %s", c.Id)
			return err
		}
		err = c.start()
		if err != nil {
			log.Printf("Error while starting container %s", c.Id)
			return err
		}
	}
	return nil
}

func (p *DockerProvisioner) Deploy(app provision.App, w io.Writer) error {
	_, err := newContainer(app)
	return err
}

func (p *DockerProvisioner) Destroy(app provision.App) error {
	units := app.ProvisionUnits()
	for _, u := range units {
		if u.GetName() != "" {
			go func(u provision.AppUnit) {
				c := container{Id: u.GetName()}
				if err := c.remove(); err != nil {
					return
				}
			}(u)
		}
	}
	return nil
}

func (*DockerProvisioner) Addr(app provision.App) (string, error) {
	r, err := getRouter()
	if err != nil {
		log.Printf("Failed to get router: %s", err.Error())
		return "", err
	}
	addr, err := r.Addr(app.GetName())
	if err != nil {
		log.Printf("Failed to obtain app %s address: %s", app.GetName(), err.Error())
		return "", err
	}
	return addr, nil
}

func (*DockerProvisioner) AddUnits(app provision.App, units uint) ([]provision.Unit, error) {
	return []provision.Unit{}, nil
}

func (*DockerProvisioner) RemoveUnit(app provision.App, unitName string) error {
	container, err := getContainer(unitName)
	if err != nil {
		return err
	}
	if container.AppName != app.GetName() {
		return errors.New("Unit does not belong to this app")
	}
	return container.remove()
}

func (*DockerProvisioner) InstallDeps(app provision.App, w io.Writer) error {
	return nil
}

func (*DockerProvisioner) ExecuteCommand(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	containers, err := getContainers(app.GetName())
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return errors.New("No containers for this app")
	}
	for _, c := range containers {
		err = c.ssh(stdout, stderr, cmd, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *DockerProvisioner) CollectStatus() ([]provision.Unit, error) {
	docker, err := config.GetString("docker:binary")
	if err != nil {
		return nil, err
	}
	out, err := runCmd(docker, "ps", "-q")
	if err != nil {
		return nil, err
	}
	var linesGroup sync.WaitGroup
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	units := make(chan provision.Unit, len(lines))
	result := buildResult(len(lines), units)
	errs := make(chan error, 1)
	for _, line := range lines {
		linesGroup.Add(1)
		go collectUnit(line, units, errs, &linesGroup)
	}
	linesGroup.Wait()
	close(errs)
	close(units)
	if err, ok := <-errs; ok {
		return nil, err
	}
	return <-result, nil
}

func collectUnit(id string, units chan<- provision.Unit, errs chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()
	docker, _ := config.GetString("docker:binary")
	container, err := getContainer(id)
	if err != nil {
		log.Printf("Container %q not in the database. Skipping...", id)
		return
	}
	out, err := runCmd(docker, "inspect", id)
	if err != nil {
		errs <- err
		return
	}
	var c map[string]interface{}
	err = json.Unmarshal([]byte(out), &c)
	if err != nil {
		errs <- err
		return
	}
	unit := provision.Unit{
		Name:    container.Id,
		AppName: container.AppName,
		Type:    container.Type,
	}
	unit.Ip = c["NetworkSettings"].(map[string]interface{})["IpAddress"].(string)
	addr := fmt.Sprintf("%s:%s", unit.Ip, container.Port)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		unit.Status = provision.StatusInstalling
	} else {
		conn.Close()
		unit.Status = provision.StatusStarted
	}
	units <- unit
}

func buildResult(maxSize int, units <-chan provision.Unit) <-chan []provision.Unit {
	ch := make(chan []provision.Unit, 1)
	go func() {
		result := make([]provision.Unit, 0, maxSize)
		for unit := range units {
			result = append(result, unit)
		}
		ch <- result
	}()
	return ch
}

func getPort(portMapping map[string]interface{}) string {
	var port string
	for k := range portMapping {
		port = k
		break
	}
	return port
}

func collection() *mgo.Collection {
	name, err := config.GetString("docker:collection")
	if err != nil {
		log.Fatalf("FATAL: %s.", err)
	}
	conn, err := db.Conn()
	if err != nil {
		log.Printf("Failed to connect to the database: %s", err)
	}
	return conn.Collection(name)
}

func imagesCollection() *mgo.Collection {
	nameIndex := mgo.Index{Key: []string{"name"}, Unique: true}
	conn, err := db.Conn()
	if err != nil {
		log.Printf("Failed to connect to the database: %s", err)
	}
	c := conn.Collection("docker_image")
	c.EnsureIndex(nameIndex)
	return c
}
