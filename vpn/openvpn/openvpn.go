package openvpn

import (
	"errors"
	"fmt"
	"ivpn/daemon/logger"
	"ivpn/daemon/obfsproxy"
	"ivpn/daemon/service/platform"
	"ivpn/daemon/shell"
	"ivpn/daemon/vpn"
	"net"
	"strings"
	"sync"
)

var log *logger.Logger

func init() {
	log = logger.NewLogger("ovpn")
}

// OpenVPN structure represents all data of OpenVPN connection
type OpenVPN struct {
	binaryPath      string
	configPath      string
	logFile         string
	isObfsProxy     bool
	extraParameters string // user-defined extra-parameters of OpenVPN configuration
	connectParams   ConnectionParams

	managementInterface *ManagementInterface
	obfsproxy           *obfsproxy.Obfsproxy

	// current VPN state
	state    vpn.State
	clientIP net.IP // applicable only for 'CONNECTED' state

	// platform-specific properties (for macOS, Windows etc. ...)
	psProps platformSpecificProperties

	// If true - the disconnection requested
	// No connection is possible anymore (to make new connection a new OpenVPN must be initialized).
	// If we are in 'connecting' state - stop
	isDisconnectRequested bool

	// Note: Disconnect() function will wait until VPN fully disconnects
	runningWG sync.WaitGroup

	isPaused bool
}

// NewOpenVpnObject creates new OpenVPN structure
func NewOpenVpnObject(
	binaryPath string,
	configPath string,
	logFile string,
	isObfsProxy bool,
	extraParameters string,
	connectionParams ConnectionParams) (*OpenVPN, error) {

	return &OpenVPN{
			state:           vpn.DISCONNECTED,
			binaryPath:      binaryPath,
			configPath:      configPath,
			logFile:         logFile,
			isObfsProxy:     isObfsProxy,
			extraParameters: extraParameters,
			connectParams:   connectionParams},
		nil
}

// DestinationIPs -  Get destination IPs (VPN host server or proxy server IP address)
// This information if required, for example, to allow this address in firewall
func (o *OpenVPN) DestinationIPs() []net.IP {
	if o.connectParams.proxyAddress != nil {
		return []net.IP{o.connectParams.proxyAddress}
	}
	return o.connectParams.hostIPs
}

// Connect - SYNCHRONOUSLY execute openvpn process (wait untill it finished)
func (o *OpenVPN) Connect(stateChan chan<- vpn.StateInfo) (retErr error) {

	// Note: Disconnect() function will wait until VPN fully disconnects
	o.runningWG.Add(1)
	// mark openVPN is fully stopped
	defer o.runningWG.Done()

	if o.isDisconnectRequested {
		return errors.New("disconnection already requested for this OpenVPN object. To make a new connection, please, initialize new one")
	}

	// it allows to wait till all routines finished
	var routinesWaiter sync.WaitGroup
	// marker to stop state-forward routine
	stopStateChan := make(chan struct{})
	// channel will be analyzed for state change. States will be forwarded to channel above ( to 'stateChan')
	intarnalStateChan := make(chan vpn.StateInfo, 1)

	// EXIT: stopping everything: Management interface, Obfsproxy
	defer func() {

		if retErr != nil {
			log.Error("Connection error: ", retErr)
		}

		// stop state-forward routine
		stopStateChan <- struct{}{}

		mi := o.managementInterface
		if mi != nil {
			if err := mi.StopManagementInterface(); err != nil {
				log.Error(err)
			}
		}

		obfspxy := o.obfsproxy
		if obfspxy != nil {
			obfspxy.Stop()
		}

		o.obfsproxy = nil

		if err := o.implOnDisconnected(); err != nil {
			log.Error(err)
		}

		// wait till all routines finished
		routinesWaiter.Wait()
	}()

	// analyse and forward state changes
	routinesWaiter.Add(1)
	go func() {
		defer routinesWaiter.Done()

		var stateInf vpn.StateInfo
		for {
			select {
			case stateInf = <-intarnalStateChan:
				// save current state
				o.state = stateInf.State

				// forward state
				stateChan <- stateInf

				if o.state == vpn.CONNECTED {
					o.clientIP = stateInf.ClientIP
					o.implOnConnected() // process "on connected" event (if necessary)
				} else {
					o.clientIP = nil
				}

			case <-stopStateChan: // openvpn process stopped
				return // stop goroutine
			}
		}
	}()

	if o.managementInterface != nil {
		return errors.New("unable to connect OpenVPN. Management interface already initialized")
	}

	var err error
	obfsproxyPort := 0
	// start Obfsproxy (if necessary)
	if o.isObfsProxy {
		o.obfsproxy = obfsproxy.CreateObfsproxy(platform.ObfsproxyStartScript())
		if obfsproxyPort, err = o.obfsproxy.Start(); err != nil {
			return errors.New("unable to initialize OpenVPN (obfsproxy not started): " + err.Error())
		}

		// detect opbfsproxy ptocess stop
		routinesWaiter.Add(1)
		go func() {
			defer routinesWaiter.Done()

			opxy := o.obfsproxy
			if opxy == nil {
				return
			}

			// wait for obfsproxy stop
			opxy.Wait()
			if o.isDisconnectRequested == false {
				// If obfsproxy stopped unexpectedly - disconnect VPN
				log.Error("Obfsproxy stopped unexpectedly. Disconnecting VPN...")
				o.doDisconnect()
			}
		}()
	}

	// start new management interface
	mi, err := StartManagementInterface(o.connectParams.username, o.connectParams.password, intarnalStateChan)
	if err != nil {
		return fmt.Errorf("failed to start MI: %w", err)
	}
	o.managementInterface = mi

	if o.isDisconnectRequested {
		// If the disconnection request received immediately after 'connect' request - stop connection after MI is initialized
		log.Info("Connection process cancelled.")
		return nil
	}

	miIP, miPort, err := mi.ListenAddress()
	if err != nil {
		return fmt.Errorf("failed to start MI listener: %w", err)
	}

	// create config file
	err = o.connectParams.WriteConfigFile(
		o.configPath,
		miIP, miPort,
		o.logFile,
		obfsproxyPort,
		o.extraParameters)

	if err != nil {
		return fmt.Errorf("failed to write configuration file: %w", err)
	}

	// SYNCHRONOUSLY execute openvpn process (wait untill it finished)
	if err = shell.Exec(log, o.binaryPath, "--config", o.configPath); err != nil {
		return fmt.Errorf("failed to start OpenVPN process: %w", err)
	}

	return nil
}

// Disconnect stops the connection
func (o *OpenVPN) Disconnect() error {

	if err := o.doDisconnect(); err != nil {
		return fmt.Errorf("disconnection error : %w", err)
	}

	// waiting untill process is running
	// (ensure all disconnection operations performed (e.g. obgsproxy is stopped, etc. ...))
	o.runningWG.Wait()

	return nil
}

func (o *OpenVPN) doDisconnect() error {

	// there is a chance we are in 'connecting' state, but managementInterface is not defined yet
	// Therefore, we are saving our intention to disconnect
	o.isDisconnectRequested = true

	mi := o.managementInterface
	if mi == nil {
		log.Error("OpenVPN MI is nil")
		return nil // nothing to disconnect
	}

	return mi.SendDisconnect()
}

// Pause doing required operation for Pause (remporary restoring default DNS)
func (o *OpenVPN) Pause() error {
	o.isPaused = true

	mi := o.managementInterface
	if mi == nil {
		return errors.New("OpenVPN MI is nil")
	}

	routeAddCommands := mi.GetRouteAddCommands()
	if len(routeAddCommands) == 0 {
		return errors.New("OpenVPN: no route-add commands detected")
	}

	var retErr error
	for _, cmd := range routeAddCommands {
		delCmd := strings.Replace(cmd, "add", "delete", -1)

		cmdCols := strings.SplitN(delCmd, " ", 2)
		if len(cmdCols) != 2 {
			retErr = errors.New("failed to parse route-change command: " + delCmd)
			log.Error(retErr.Error())
			continue
		}

		arguments := strings.Split(cmdCols[1], " ")
		if err := shell.Exec(log, cmdCols[0], arguments...); err != nil {
			retErr = err
			log.Error(err)
		}
	}

	// OS-specific operation (if required)
	retErr = o.implOnPause()
	if retErr != nil {
		log.ErrorTrace(retErr)
	}

	return retErr
}

// Resume doing required operation for Resume (restores DNS configuration before Pause)
func (o *OpenVPN) Resume() error {
	defer func() {
		o.isPaused = false
	}()

	mi := o.managementInterface
	if mi == nil {
		return errors.New("OpenVPN MI is nil")
	}

	routeAddCommands := mi.GetRouteAddCommands()
	if len(routeAddCommands) == 0 {
		return errors.New("OpenVPN: no route-add commands detected")
	}

	var retErr error
	for _, cmd := range routeAddCommands {
		cmdCols := strings.SplitN(cmd, " ", 2)
		if len(cmdCols) != 2 {
			retErr = errors.New("failed to parse route-change command: " + cmd)
			log.Error(retErr.Error())
			continue
		}

		arguments := strings.Split(cmdCols[1], " ")
		if err := shell.Exec(log, cmdCols[0], arguments...); err != nil {
			retErr = err
			log.Error(err)
		}
	}

	// OS-specific operation (if required)
	retErr = o.implOnResume()
	if retErr != nil {
		log.ErrorTrace(retErr)
	}

	return retErr
}

// IsPaused checking if we are in paused state
func (o *OpenVPN) IsPaused() bool {
	return o.isPaused
}

// SetManualDNS changes DNS to manual IP
func (o *OpenVPN) SetManualDNS(addr net.IP) error {
	return o.implOnSetManualDNS(addr)
}

// ResetManualDNS restores DNS
func (o *OpenVPN) ResetManualDNS() error {
	return o.implOnResetManualDNS()
}
