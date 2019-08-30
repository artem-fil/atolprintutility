package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/google/uuid"
	"github.com/kardianos/service"
	"github.com/sacOO7/gowebsocket"
)

var addr = flag.String("addr", "localhost", "http service address")
var atolServerURL = "http://localhost:16732/requests"
var logPath = "log.txt"
var getRequestDelay = time.Duration(3)
var getRequestAttempts = 6
var logger service.Logger

type program struct{}
type getRespType struct {
	Results []result
}
type result struct {
	Status           string
	ErrorCode        int
	ErrorDescription string
	Result           interface{}
}
type getShiftRespType struct {
	Results []shiftResult
}
type shiftResult struct {
	Status           string
	ErrorCode        int
	ErrorDescription string
	Result           shift
}
type shift struct {
	ShiftStatus shiftStatus
}
type shiftStatus struct {
	ExpiredTime string
	Number      int
	State       string
}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}
func (p *program) run() {
	flag.Parse()
	log.SetFlags(0)

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	u := url.URL{Scheme: "wss", Host: *addr, Path: "/websocket"}
	writeToLog("self: ", "Connecting to ", u.String())
	var connected = false
	socket := gowebsocket.New(u.String())

	socket.OnConnected = func(socket gowebsocket.Socket) {
		connected = true
		writeToLog("self: ", "Ready")
	}

	socket.OnConnectError = func(err error, socket gowebsocket.Socket) {
		connected = false
		writeToLog("wsserver: ", "Error while connecting: ", err)
	}

	socket.OnTextMessage = func(message string, socket gowebsocket.Socket) {

		writeToLog("self: ", " ======= New task ======== ")
		// checking Shift status, close and open if required
		writeToLog("self: ", "1. Checking Shift status... ")
		err := handleShiftStatus()
		if err != nil {
			errorHandle(socket, "self: ", "Error while handling Shift status: ", err.Error())
			return
		}
		writeToLog("self: ", "2. Sending JSON task... ")
		// sending POST req with JSON task
		newUUID, err := uuid.NewUUID()
		if err != nil {
			errorHandle(socket, "self: ", "Error while uuid generation: ", err.Error())
			return
		}
		var r interface{}
		err = json.Unmarshal([]byte(message), &r)
		if err != nil {
			errorHandle(socket, "self: ", "Error while task unmarshaling: ", err.Error())
			return
		}
		writeToLog("self: ", "Incoming JSON: ", message)

		postResp, err := postRequest(newUUID.String(), r)
		if err != nil {
			errorHandle(socket, "atol-webserver: ", "Error while sending POST: ", err.Error())
			return
		}
		defer postResp.Body.Close()
		writeToLog("atol-webserver: ", "POST response status: ", postResp.Status)

		if postResp.StatusCode == http.StatusCreated {
			writeToLog("self: ", "3. GETting result... ")
			//here we need to loop this several times until task isnt finished

			for i := 0; i < getRequestAttempts; i++ {
				getResp, err := http.Get(fmt.Sprintf("%s/%s", atolServerURL, newUUID.String()))
				if err != nil {
					writeToLog("atol-webserver: ", "Error while sending GET: ", err)
				}

				defer getResp.Body.Close()
				if getResp.StatusCode == http.StatusOK {

					getRespBody, _ := ioutil.ReadAll(getResp.Body)
					writeToLog("atol-webserver: ", "GET response Body:", string(getRespBody))

					var getRespBodyStruct getRespType
					err = json.Unmarshal(getRespBody, &getRespBodyStruct)
					if err != nil {
						writeToLog("self: ", "Error while GET response JSON unmarshaling: ", err)
					}
					result := getRespBodyStruct.Results[0]

					if result.Status == "ready" {
						writeToLog("atol-webserver: ", "Everything seems to be OK")
						break
					} else {
						writeToLog("atol-webserver: ", "Task wasn't printed. Status: ", result.Status, ". Error code: ", result.ErrorCode, ". Error description: ", result.ErrorDescription)
					}
				} else {
					writeToLog("atol-webserver: ", "Failed GET request: ", getResp.Status)
				}
				time.Sleep(getRequestDelay * time.Second)
			}
		} else {
			errorHandle(socket, "atol-webserver: ", "Failed POST request", "Task from POST request wasn't added to queue")
		}
		writeToLog("self: ", " ======= Task finished ======== ")
	}

	socket.OnPingReceived = func(data string, socket gowebsocket.Socket) {
		socket.SendText("heartbeat")
	}

	socket.OnDisconnected = func(err error, socket gowebsocket.Socket) {
		writeToLog("wsserver: ", "Disconnected from server")
		return
	}
	for !connected {
		socket.Connect()
		time.Sleep(time.Second)
	}

	for {
		select {
		case <-interrupt:
			log.Println("Interrupt")
			socket.Close()
			return
		}
	}
}
func (p *program) Stop(s service.Service) error {
	return nil
}

func main() {
	svcConfig := &service.Config{
		Name:        "AtolPrintService",
		DisplayName: "ATOL print service",
		Description: "Service for communication between atol web-server and remote nodejs application",
	}

	prg := &program{}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}
	logger, err = s.Logger(nil)
	if err != nil {
		log.Fatal(err)
	}
	err = s.Run()
	if err != nil {
		logger.Error(err)
	}
}

func handleShiftStatus() error {

	newUUID, err := uuid.NewUUID()
	if err != nil {
		return fmt.Errorf("Error while uuid generation: %s", err.Error())
	}
	var r = struct {
		Type string `json:"type"`
	}{"getShiftStatus"}
	postResp, err := postRequest(newUUID.String(), r)
	if err != nil {
		return fmt.Errorf("Error while sending POST: %s", err.Error())
	}
	defer postResp.Body.Close()

	if postResp.StatusCode == http.StatusCreated {
		for i := 0; i < getRequestAttempts; i++ {
			getResp, err := http.Get(fmt.Sprintf("%s/%s", atolServerURL, newUUID.String()))
			if err != nil {
				return fmt.Errorf("Error while sending GET: %s", err.Error())
			}
			defer getResp.Body.Close()

			if getResp.StatusCode == http.StatusOK {
				getRespBody, _ := ioutil.ReadAll(getResp.Body)

				var getRespBodyStruct getShiftRespType
				err = json.Unmarshal(getRespBody, &getRespBodyStruct)
				if err != nil {
					return fmt.Errorf("Error while GET response JSON unmarshaling: %s", err.Error())
				}

				response := getRespBodyStruct.Results[0]

				if response.Status == "ready" {
					writeToLog("atol-webserver: ", " Shift State: ", response.Result.ShiftStatus.State)
					if response.Result.ShiftStatus.State == "opened" {
						writeToLog("atol-webserver: ", " Shift is opened. Proceed to main procedure")
						return nil
					}
					// if Shift is expired - we need to close it
					if response.Result.ShiftStatus.State == "expired" {
						writeToLog("atol-webserver: ", " Shift was expired. Trying to close it...")
						newUUID2, err := uuid.NewUUID()
						if err != nil {
							return fmt.Errorf("Error while uuid generation: %s", err.Error())
						}
						var r = struct {
							Type string `json:"type"`
						}{"closeShift"}
						postResp, err := postRequest(newUUID2.String(), r)
						if err != nil {
							return fmt.Errorf("Error while sending POST: %s", err.Error())
						}
						defer postResp.Body.Close()

						if postResp.StatusCode == http.StatusCreated {
							for i := 0; i < getRequestAttempts; i++ {
								getResp, err := http.Get(fmt.Sprintf("%s/%s", atolServerURL, newUUID2.String()))
								if err != nil {
									return fmt.Errorf("Error while sending GET: %s", err.Error())
								}
								defer getResp.Body.Close()

								if getResp.StatusCode == http.StatusOK {
									getRespBody, _ := ioutil.ReadAll(getResp.Body)
									var getRespBodyStruct getRespType
									err = json.Unmarshal(getRespBody, &getRespBodyStruct)
									if err != nil {
										return fmt.Errorf("Error while GET response JSON unmarshaling: %s", err.Error())
									}
									response := getRespBodyStruct.Results[0]
									if response.Status == "ready" {
										writeToLog("atol-webserver: ", "Shift has been closed and should be opened automatically.")
										return nil
									}
									time.Sleep(getRequestDelay * time.Second)
								}
							}
							return fmt.Errorf("Closing shift task failed. All attempts are timed out")
						}
						return fmt.Errorf("Task from POST request wasn't added to queue: %s", postResp.Status)
					}
				} else {
					time.Sleep(getRequestDelay * time.Second)
				}
			} else {
				return fmt.Errorf("GET result of getShitStatus failed")
			}
		}
		return fmt.Errorf("Get shift status task failed. All attempts are timed out")
	}
	return fmt.Errorf("POST getShitStatus task wasn't added to queue: %s", postResp.Status)
}

func postRequest(newUUID string, request interface{}) (*http.Response, error) {

	postReqBody := map[string]interface{}{"uuid": newUUID, "request": request}
	postJSON, err := json.Marshal(postReqBody)
	if err != nil {
		return nil, fmt.Errorf("Error while JSON marshaling: %s", err.Error())
	}
	req, err := http.NewRequest("POST", atolServerURL, bytes.NewBuffer(postJSON))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	postResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Error while sending POST: %s", err.Error())
	}
	return postResp, nil
}

func writeToLog(prefix string, a ...interface{}) {

	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err.Error())
	}
	defer file.Close()

	logger := log.New(file, prefix, log.LstdFlags)
	logger.Println(a...)
}

func errorHandle(socket gowebsocket.Socket, level, title, body string) {
	writeToLog(level, title, body)
	errJSON, _ := json.Marshal(map[string]string{"type": "error", "message": fmt.Sprintf("%s | %s", title, body)})
	socket.SendText(string(errJSON))
}
