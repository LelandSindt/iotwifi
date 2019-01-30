// IoT Wifi packages is used to manage WiFi AP and Station (client) modes on
// a Raspberry Pi or other arm device. This code is intended to run in it's
// corresponding Alpine docker container.

package iotwifi

import (
	"bufio"
	"encoding/json"
	"github.com/bhoriuchi/go-bunyan/bunyan"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"time"
)

// CmdRunner runs internal commands allows output handlers to be attached.
type CmdRunner struct {
	Log      bunyan.Logger
	Messages chan CmdMessage
	Handlers map[string]func(CmdMessage)
	Commands map[string]*exec.Cmd
}

// CmdMessage structures command output.
type CmdMessage struct {
	Id      string
	Command string
	Message string
	Error   bool
	Cmd     *exec.Cmd
	Stdin   *io.WriteCloser
}

// loadCfg loads the configuration.
func loadCfg(cfgLocation string) (*SetupCfg, error) {

	v := &SetupCfg{}
	var jsonData []byte

	urlDelimR, _ := regexp.Compile("://")
	isUrl := urlDelimR.Match([]byte(cfgLocation))

	// if not a url
	if !isUrl {
		fileData, err := ioutil.ReadFile(cfgLocation)
		if err != nil {
			panic(err)
		}
		jsonData = fileData
	}

	if isUrl {
		res, err := http.Get(cfgLocation)
		if err != nil {
			panic(err)
		}

		defer res.Body.Close()

		urlData, err := ioutil.ReadAll(res.Body)
		if err != nil {
			panic(err)
		}

		jsonData = urlData
	}

	err := json.Unmarshal(jsonData, v)

	return v, err
}

// the more I think about this, I think that there should be two go functions/threads
// one to run wpa_supplicant (wlan0) and watch wlan0's connection state and
// bring up uap0, hostapd, dnsmasq
// take down uap0, hostapd, dnsmasq...
// and a second go function/thread to handle message logging.

func RunWifi(log bunyan.Logger, messages chan CmdMessage, cfgLocation string) {
	staticFields := make(map[string]interface{})
	lastInterfaceState := "none"
	curInterfaceState := "none"
	loopcount := 0
	isApOn := false

	log.Info("Loading IoT Wifi...")

	cmdRunner := CmdRunner{
		Log:      log,
		Messages: messages,
		Handlers: make(map[string]func(cmsg CmdMessage), 0),
		Commands: make(map[string]*exec.Cmd, 0),
	}

	setupCfg, err := loadCfg(cfgLocation)
	if err != nil {
		log.Error("Could not load config: %s", err.Error())
		return
	}

	command := &Command{
		Log:      log,
		Runner:   cmdRunner,
		SetupCfg: setupCfg,
	}

	wpacfg := NewWpaCfg(log, cfgLocation)

	command.RemoveApInterface()
	command.StartWpaSupplicant() //wpa_supplicant

	for {
		// if interfaceState(wlan0) == "CONNECTED" then { stop uap0 } else { start uap0 }

		staticFields["cmd_id"] = "State change"

		curInterfaceState = interfaceState("wlan0")
		if lastInterfaceState != curInterfaceState {
			log.Info(staticFields, "Begin: " + curInterfaceState)
			loopcount = 0
			for {
				curInterfaceState = interfaceState("wlan0")
				if lastInterfaceState == curInterfaceState {
					break
				} else {
					//todo: change this to a sane number ?6? -- 30 seconds totoal
					if loopcount > 5 {
						lastInterfaceState = interfaceState("wlan0")
						log.Info(staticFields, "New: " + lastInterfaceState)
						if lastInterfaceState == "COMPLETED" {
							if isApOn == true{
								log.Info(staticFields, "Turn off AP")
								command.stopHostapd()
								command.stopDnsmasq()
								command.RemoveApInterface()
								isApOn = false
							}
						} else {
							if isApOn == false {
								log.Info(staticFields, "Turn on AP")
								command.stopWpaSupplicant() //Todo: not entirely sure this is required, test further.
								time.Sleep(1 * time.Second)
								wpacfg.StartAP() //hostapd
								time.Sleep(1 * time.Second)
								command.StartWpaSupplicant()
								time.Sleep(1 * time.Second)
								command.StartDnsmasq() //dnsmasq
								isApOn = true
							}
						}
						break
					}
				log.Info(staticFields, "Transition: " + lastInterfaceState + " -> " + curInterfaceState)
				loopcount ++
				//todo: change this to a sane number ?5 seconds? -- 30 seconds total
				time.Sleep(1 * time.Second)
				}
			}
		}
		//Todo: change this to a sane number -- ?30? seconds?
		time.Sleep(5 * time.Second)
	}
}


// RunWifi starts AP and Station modes.
func HandleLog(log bunyan.Logger, messages chan CmdMessage) {

	cmdRunner := CmdRunner{
		Log:      log,
		Messages: messages,
		Handlers: make(map[string]func(cmsg CmdMessage), 0),
		Commands: make(map[string]*exec.Cmd, 0),
	}

	// listen to kill messages
	cmdRunner.HandleFunc("kill", func(cmsg CmdMessage) {
		log.Error("GOT KILL")
		os.Exit(1)
	})

	// staticFields for logger
	staticFields := make(map[string]interface{})

	// command output loop (channel messages)
	// loop and log
	//
	for {
		out := <-messages // Block until we receive a message on the channel

		staticFields["cmd_id"] = out.Id
		staticFields["cmd"] = out.Command
		staticFields["is_error"] = out.Error

		log.Info(staticFields, out.Message)

		if handler, ok := cmdRunner.Handlers[out.Id]; ok {
			handler(out)
		}
	}
}

// HandleFunc is a function that gets all channel messages for a command id
func (c *CmdRunner) HandleFunc(cmdId string, handler func(cmdMessage CmdMessage)) {
	c.Handlers[cmdId] = handler
}

// ProcessCmd processes an internal command.
func (c *CmdRunner) ProcessCmd(id string, cmd *exec.Cmd) {
	c.Log.Debug("ProcessCmd got %s", id)

	// add command to the commands map TODO close the readers
	c.Commands[id] = cmd

	cmdStdoutReader, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}

	cmdStderrReader, err := cmd.StderrPipe()
	if err != nil {
		panic(err)
	}

	stdOutScanner := bufio.NewScanner(cmdStdoutReader)
	go func() {
		for stdOutScanner.Scan() {
			c.Messages <- CmdMessage{
				Id:      id,
				Command: cmd.Path,
				Message: stdOutScanner.Text(),
				Error:   false,
				Cmd:     cmd,
			}
		}
	}()

	stdErrScanner := bufio.NewScanner(cmdStderrReader)
	go func() {
		for stdErrScanner.Scan() {
			c.Messages <- CmdMessage{
				Id:      id,
				Command: cmd.Path,
				Message: stdErrScanner.Text(),
				Error:   true,
				Cmd:     cmd,
			}
		}
	}()

	err = cmd.Start()

	if err != nil {
		panic(err)
	}

	c.Log.Debug("ProcessCmd waiting %s", id)
	err = cmd.Wait()
	c.Log.Debug("ProcessCmd done %s got %s ", id, err)

}
