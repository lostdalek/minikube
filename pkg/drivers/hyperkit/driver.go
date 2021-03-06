// +build darwin

/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hyperkit

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mitchellh/go-ps"

	"io/ioutil"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/state"
	"github.com/johanneswuerbach/nfsexports"
	"github.com/moby/hyperkit/go"
	"github.com/pkg/errors"
	pkgdrivers "k8s.io/minikube/pkg/drivers"
	"k8s.io/minikube/pkg/minikube/constants"
	commonutil "k8s.io/minikube/pkg/util"
)

const (
	isoFilename     = "boot2docker.iso"
	pidFileName     = "hyperkit.pid"
	machineFileName = "hyperkit.json"
	permErr         = "%s needs to run with elevated permissions. " +
		"Please run the following command, then try again: " +
		"sudo chown root:wheel %s && sudo chmod u+s %s"
)

type Driver struct {
	*drivers.BaseDriver
	*pkgdrivers.CommonDriver
	Boot2DockerURL string
	DiskSize       int
	CPU            int
	Memory         int
	Cmdline        string
	NFSShares      []string
	NFSSharesRoot  string
	UUID           string
	VpnKitSock     string
	VSockPorts     []string
}

func NewDriver(hostName, storePath string) *Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			SSHUser: "docker",
		},
		CommonDriver: &pkgdrivers.CommonDriver{},
		DiskSize:     commonutil.CalculateDiskSizeInMB(constants.DefaultDiskSize),
	}
}

// PreCreateCheck is called to enforce pre-creation steps
func (d *Driver) PreCreateCheck() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	if syscall.Geteuid() != 0 {
		return fmt.Errorf(permErr, filepath.Base(exe), exe, exe)
	}

	return nil
}

func (d *Driver) Create() error {
	// TODO: handle different disk types.
	if err := pkgdrivers.MakeDiskImage(d.BaseDriver, d.Boot2DockerURL, d.DiskSize); err != nil {
		return errors.Wrap(err, "making disk image")
	}

	isoPath := d.ResolveStorePath(isoFilename)
	if err := d.extractKernel(isoPath); err != nil {
		return errors.Wrap(err, "extracting kernel")
	}

	return d.Start()
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return "hyperkit"
}

// GetSSHHostname returns hostname for use with ssh
func (d *Driver) GetSSHHostname() (string, error) {
	return d.IPAddress, nil
}

// GetURL returns a Docker compatible host URL for connecting to this host
// e.g. tcp://1.2.3.4:2376
func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

// GetState returns the state that the host is in (running, stopped, etc)
func (d *Driver) GetState() (state.State, error) {
	pid := d.getPid()
	if pid == 0 {
		return state.Stopped, nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return state.Error, err
	}

	// Sending a signal of 0 can be used to check the existence of a process.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return state.Stopped, nil
	}
	if p == nil {
		return state.Stopped, nil
	}
	return state.Running, nil
}

// Kill stops a host forcefully
func (d *Driver) Kill() error {
	return d.sendSignal(syscall.SIGKILL)
}

// Remove a host
func (d *Driver) Remove() error {
	s, err := d.GetState()
	if err != nil || s == state.Error {
		log.Infof("Error checking machine status: %v, assuming it has been removed already", err)
	}
	if s == state.Running {
		if err := d.Stop(); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) Restart() error {
	return pkgdrivers.Restart(d)
}

// Start a host
func (d *Driver) Start() error {
	stateDir := filepath.Join(d.StorePath, "machines", d.MachineName)
	if err := d.recoverFromUncleanShutdown(); err != nil {
		return err
	}
	h, err := hyperkit.New("", d.VpnKitSock, stateDir)
	if err != nil {
		return errors.Wrap(err, "new-ing Hyperkit")
	}

	// TODO: handle the rest of our settings.
	h.Kernel = d.ResolveStorePath("bzimage")
	h.Initrd = d.ResolveStorePath("initrd")
	h.VMNet = true
	h.ISOImages = []string{d.ResolveStorePath(isoFilename)}
	h.Console = hyperkit.ConsoleFile
	h.CPUs = d.CPU
	h.Memory = d.Memory
	h.UUID = d.UUID

	if vsockPorts, err := d.extractVSockPorts(); err != nil {
		return err
	} else if len(vsockPorts) >= 1 {
		h.VSock = true
		h.VSockPorts = vsockPorts
	}

	log.Infof("Using UUID %s", h.UUID)
	mac, err := GetMACAddressFromUUID(h.UUID)
	if err != nil {
		return errors.Wrap(err, "getting MAC address from UUID")
	}

	// Need to strip 0's
	mac = trimMacAddress(mac)
	log.Infof("Generated MAC %s", mac)
	h.Disks = []hyperkit.DiskConfig{
		{
			Path:   pkgdrivers.GetDiskPath(d.BaseDriver),
			Size:   d.DiskSize,
			Driver: "virtio-blk",
		},
	}
	log.Infof("Starting with cmdline: %s", d.Cmdline)
	if err := h.Start(d.Cmdline); err != nil {
		return errors.Wrapf(err, "starting with cmd line: %s", d.Cmdline)
	}

	getIP := func() error {
		var err error
		d.IPAddress, err = GetIPAddressByMACAddress(mac)
		if err != nil {
			return &commonutil.RetriableError{Err: err}
		}
		return nil
	}

	if err := commonutil.RetryAfter(30, getIP, 2*time.Second); err != nil {
		return fmt.Errorf("IP address never found in dhcp leases file %v", err)
	}

	if len(d.NFSShares) > 0 {
		log.Info("Setting up NFS mounts")
		// takes some time here for ssh / nfsd to work properly
		time.Sleep(time.Second * 30)
		err = d.setupNFSShare()
		if err != nil {
			// TODO(tstromberg): Check that logging an and error and return it is appropriate. Seems weird.
			log.Errorf("NFS setup failed: %v", err)
			return err
		}
	}

	return nil
}

//recoverFromUncleanShutdown searches for an existing hyperkit.pid file in
//the machine directory. If it can't find it, a clean shutdown is assumed.
//If it finds the pid file, it checks for a running hyperkit process with that pid
//as the existence of a file might not indicate an unclean shutdown but an actual running
//hyperkit server. This is an error situation - we shouldn't start minikube as there is likely
//an instance running already. If the PID in the pidfile does not belong to a running hyperkit
//process, we can safely delete it, and there is a good chance the machine will recover when restarted.
func (d *Driver) recoverFromUncleanShutdown() error {
	stateDir := filepath.Join(d.StorePath, "machines", d.MachineName)
	pidFile := filepath.Join(stateDir, pidFileName)

	_, err := os.Stat(pidFile)

	if os.IsNotExist(err) {
		log.Infof("clean start, hyperkit pid file doesn't exist: %s", pidFile)
		return nil
	}

	if err != nil {
		return errors.Wrap(err, "checking hyperkit pid file existence")
	}

	log.Warnf("minikube might have been shutdown in an unclean way, the hyperkit pid file still exists: %s", pidFile)

	content, err := ioutil.ReadFile(pidFile)
	if err != nil {
		return errors.Wrapf(err, "reading pidfile %s", pidFile)
	}
	pid, err := strconv.Atoi(string(content))
	if err != nil {
		return errors.Wrapf(err, "parsing pidfile %s", pidFile)
	}

	p, err := ps.FindProcess(pid)
	if err != nil {
		return errors.Wrapf(err, "trying to find process for PID %s", pid)
	}

	if p != nil && !strings.Contains(p.Executable(), "hyperkit") {
		return fmt.Errorf("something is not right...please stop all minikube instances, seemingly a hyperkit server is already running with pid %d, executable: %s", pid, p.Executable())
	}

	log.Infof("No running hyperkit process found with PID %d, removing %s...", pid, pidFile)
	if err := os.Remove(pidFile); err != nil {
		return errors.Wrap(err, fmt.Sprintf("removing pidFile %s", pidFile))
	}

	return nil
}

// Stop a host gracefully
func (d *Driver) Stop() error {
	d.cleanupNfsExports()
	return d.sendSignal(syscall.SIGTERM)
}

func (d *Driver) extractKernel(isoPath string) error {
	for _, f := range []struct {
		pathInIso string
		destPath  string
	}{
		{"/boot/bzimage", "bzimage"},
		{"/boot/initrd", "initrd"},
		{"/isolinux/isolinux.cfg", "isolinux.cfg"},
	} {
		fullDestPath := d.ResolveStorePath(f.destPath)
		if err := ExtractFile(isoPath, f.pathInIso, fullDestPath); err != nil {
			return err
		}
	}
	return nil
}

// InvalidPortNumberError implements the Error interface.
// It is used when a VSockPorts port number cannot be recognised as an integer.
type InvalidPortNumberError string

// Error returns an Error for InvalidPortNumberError
func (port InvalidPortNumberError) Error() string {
	return fmt.Sprintf("vsock port '%s' is not an integer", string(port))
}

func (d *Driver) extractVSockPorts() ([]int, error) {
	vsockPorts := make([]int, 0, len(d.VSockPorts))

	for _, port := range d.VSockPorts {
		p, err := strconv.Atoi(port)
		if err != nil {
			var err InvalidPortNumberError
			err = InvalidPortNumberError(port)
			return nil, err
		}
		vsockPorts = append(vsockPorts, p)
	}

	return vsockPorts, nil
}

func (d *Driver) setupNFSShare() error {
	user, err := user.Current()
	if err != nil {
		return err
	}

	hostIP, err := GetNetAddr()
	if err != nil {
		return err
	}

	mountCommands := fmt.Sprintf("#/bin/bash\\n")
	log.Info(d.IPAddress)

	for _, share := range d.NFSShares {
		if !path.IsAbs(share) {
			share = d.ResolveStorePath(share)
		}
		nfsConfig := fmt.Sprintf("%s %s -alldirs -mapall=%s", share, d.IPAddress, user.Username)

		if _, err := nfsexports.Add("", d.nfsExportIdentifier(share), nfsConfig); err != nil {
			if strings.Contains(err.Error(), "conflicts with existing export") {
				log.Info("Conflicting NFS Share not setup and ignored:", err)
				continue
			}
			return err
		}

		root := d.NFSSharesRoot
		mountCommands += fmt.Sprintf("sudo mkdir -p %s/%s\\n", root, share)
		mountCommands += fmt.Sprintf("sudo mount -t nfs -o noacl,async %s:%s %s/%s\\n", hostIP, share, root, share)
	}

	if err := nfsexports.ReloadDaemon(); err != nil {
		return err
	}

	writeScriptCmd := fmt.Sprintf("echo -e \"%s\" | sh", mountCommands)

	if _, err := drivers.RunSSHCommandFromDriver(d, writeScriptCmd); err != nil {
		return err
	}

	return nil
}

func (d *Driver) nfsExportIdentifier(path string) string {
	return fmt.Sprintf("minikube-hyperkit %s-%s", d.MachineName, path)
}

func (d *Driver) sendSignal(s os.Signal) error {
	pid := d.getPid()
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	return proc.Signal(s)
}

func (d *Driver) getPid() int {
	pidPath := d.ResolveStorePath(machineFileName)

	f, err := os.Open(pidPath)
	if err != nil {
		log.Warnf("Error reading pid file: %v", err)
		return 0
	}
	dec := json.NewDecoder(f)
	config := hyperkit.HyperKit{}
	if err := dec.Decode(&config); err != nil {
		log.Warnf("Error decoding pid file: %v", err)
		return 0
	}

	return config.Pid
}

func (d *Driver) cleanupNfsExports() {
	if len(d.NFSShares) > 0 {
		log.Infof("You must be root to remove NFS shared folders. Please type root password.")
		for _, share := range d.NFSShares {
			if _, err := nfsexports.Remove("", d.nfsExportIdentifier(share)); err != nil {
				log.Errorf("failed removing nfs share (%s): %v", share, err)
			}
		}

		if err := nfsexports.ReloadDaemon(); err != nil {
			log.Errorf("failed to reload the nfs daemon: %v", err)
		}
	}
}
