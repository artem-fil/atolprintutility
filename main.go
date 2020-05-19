package main

import (
	"encoding/json"
	"strconv"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"os/signal"
	"time"

	fptr10 "atol.ru/drivers10/fptr"
	"github.com/kardianos/service"
	"github.com/sacOO7/gowebsocket"
)
const CONNECTION_ATTEMPTS = 5;
var addr = flag.String("addr", "online.autolombard-petersburg.ru", "http service address")
var logPath = "C:/Program Files/pawnshop/log.txt"
var office = os.Getenv("OFFICE")
var logger service.Logger

type program struct{}

type Side string

const (
	Credit Side = "debit"
	Debit  Side = "credit"
)

type incomingMessage struct {
	Type      string  `json:"type"`
	Device    string  `json:"device"`
	IncomeSum float64 `json:"sum"`
	Body      struct {
		Operator string
		Side     Side
		Product  struct {
			Name     string
			Price    int32
			Quantity int8
		}
		Payments []struct {
			Type string
			Sum  int32
		}
	} `json:"body"`
}

const PAWNSHOP_SERIAL = "00106101867206"
const LEASING_SERIAL = "00106101867521"

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) Stop(s service.Service) error {
	return nil
}

func (p *program) run() {

	flag.Parse()
	log.SetFlags(0)

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Define ports
	pawnshopPort, leasingPort := definePorts();
	writeToLog("self: ", "Ports: ", pawnshopPort, leasingPort)
	// Establish websocket server connection

	u := url.URL{Scheme: "wss", Host: *addr, Path: "/websocket"}
	writeToLog("self: ", "Connecting to ", u.String())
	var connected = false
	socket := gowebsocket.New(u.String())

	// Define websocket methods

	socket.OnConnected = func(socket gowebsocket.Socket) {
		connected = true
		writeToLog("self: ", "Ready")
	}

	socket.OnConnectError = func(err error, socket gowebsocket.Socket) {
		connected = false
		writeToLog("wsserver: ", "Error while connecting: ", err)
	}

	socket.OnPingReceived = func(data string, socket gowebsocket.Socket) {
		socket.SendText("heartbeat")
	}

	socket.OnDisconnected = func(err error, socket gowebsocket.Socket) {
		writeToLog("wsserver: ", "Disconnected from server. Retry connection")
		connected = false
		for !connected {
			socket.Connect()
			time.Sleep(time.Second)
		}
		return
	}

	socket.OnTextMessage = func(message string, socket gowebsocket.Socket) {
		writeToLog("self: ", " ******* Incoming message... ******* ")
		// first we need to parse message and deal with its type
		var parsedMessage incomingMessage
		err := json.Unmarshal([]byte(message), &parsedMessage)
		if err != nil {
			writeToLog("self: ", "Incoming JSON: ", message)
			errorHandle(socket, "self: ", "Error while incoming message unmarshalling: ", err.Error())
			return
		}
		switch parsedMessage.Type {
		case "auth":
			writeToLog("self: ", " ======= Auth starting ======== ")
			// we need to send back "office" variable to bind it with id and thus be able
			// to send tasks to particular machine
			writeToLog("self: ", "OFFICE: ", office)
			authJSON, _ := json.Marshal(map[string]string{"type": "auth", "message": office})
			socket.SendText(string(authJSON))
			writeToLog("self: ", " ======= Auth completed ======== ")
		case "status":
			writeToLog("self: ", " ======= Status request ======== ")

			leasingDevice, leasingErr := connectDevice(leasingPort)
			pawnshopDevice, pawnshopErr := connectDevice(pawnshopPort)

			var pawnshopBalance, leasingBalance float64 = 0, 0

			if leasingErr == nil {
				leasingDevice.SetParam(fptr10.LIBFPTR_PARAM_DATA_TYPE, fptr10.LIBFPTR_DT_CASH_SUM)
				leasingDevice.QueryData()

				leasingBalance = leasingDevice.GetParamDouble(fptr10.LIBFPTR_PARAM_SUM)
			}

			if pawnshopErr == nil {
				pawnshopDevice.SetParam(fptr10.LIBFPTR_PARAM_DATA_TYPE, fptr10.LIBFPTR_DT_CASH_SUM)
				pawnshopDevice.QueryData()

				pawnshopBalance = pawnshopDevice.GetParamDouble(fptr10.LIBFPTR_PARAM_SUM)
			}

			type Message struct {
				Pawnshop        bool    `json:"pawnshop"`
				PawnshopBalance float64 `json:"pawnshopBalance"`
				Leasing         bool    `json:"leasing"`
				LeasingBalance  float64 `json:"leasingBalance"`
			}
			type Status struct {
				Type    string  `json:"type"`
				Message Message `json:"message"`
			}
			statusJSON, _ := json.Marshal(Status{
				Type: "status", 
				Message: Message {
					Pawnshop: pawnshopErr == nil,
					PawnshopBalance: pawnshopBalance,
					Leasing: leasingErr == nil,
					LeasingBalance: leasingBalance,
				},
			});
			socket.SendText(string(statusJSON))
			if err := leasingDevice.Close(); err != nil {
				writeToLog("self: ", "Error while leasing device closing: ", err.Error())
			}
			if err := pawnshopDevice.Close(); err != nil {
				writeToLog("self: ", "Error while pawnshop device closing: ", err.Error())
			}
			writeToLog("self: ", " ======= Status provided ======== ")
		case "income":
			writeToLog("self: ", " ======= Cash income request ======== ")

			var port string
			switch parsedMessage.Device {
			case "pawnshop":
				port = pawnshopPort
			case "leasing":
				port = leasingPort
			default:
				errorHandle(socket, "self: ", "Error while device defining: ", "device wasn't provided in the task")
				return
			}
			writeToLog("self: ", "Port: ", port)
			device, err := connectDevice(port)
			if err != nil {
				errorHandle(socket, "self: ", "Error while device connecting: ", err.Error())
				return
			}

			// checking Shift state, close if expired

			writeToLog("self: ", "1. Checking shift state ")

			device.SetParam(fptr10.LIBFPTR_PARAM_DATA_TYPE, fptr10.LIBFPTR_DT_SHIFT_STATE)
			device.QueryData()

			shiftState := device.GetParamInt(fptr10.LIBFPTR_PARAM_SHIFT_STATE)

			if shiftState == fptr10.LIBFPTR_SS_EXPIRED {
				writeToLog("self: ", "1.1 Closing expired Shift")
				device.SetParam(fptr10.LIBFPTR_PARAM_REPORT_TYPE, fptr10.LIBFPTR_RT_CLOSE_SHIFT)

				if err = device.Report(); err != nil {
					errorHandle(socket, "self: ", "Error while shift closing: ", err.Error())
					device.Close()
					return
				}

				if err = device.CheckDocumentClosed(); err != nil {
					errorHandle(socket, "self: ", "Error while document closing check: ", err.Error())
					device.Close()
					return
				}
			}

			writeToLog("self: ", "2. Applying income ")

			device.SetParam(fptr10.LIBFPTR_PARAM_SUM, parsedMessage.IncomeSum)
			if err := device.CashIncome(); err != nil {
				writeToLog("self: ", "Error while cash income method applying: ", err.Error())
			}

			if err := device.Close(); err != nil {
				writeToLog("self: ", "Error while device connection closing: ", err.Error())
			}

			successJSON, _ := json.Marshal(map[string]string{"type": "success", "message": "Everything seems to be OK"})
			socket.SendText(string(successJSON))
			
			writeToLog("self: ", " ======= Cash incomed ======== ")
		case "task":
			writeToLog("self: ", " ======= New task ======= ")
			// define what device we should looking for and establish its connection
			var port string
			// TODO: how to get rid of hardcoded port numbers
			switch parsedMessage.Device {
			case "pawnshop":
				port = pawnshopPort
			case "leasing":
				port = leasingPort
			default:
				errorHandle(socket, "self: ", "Error while device defining: ", "device wasn't provided in the task")
				return
			}
			writeToLog("self: ", "Port: ", port)
			device, err := connectDevice(port)
			if err != nil {
				errorHandle(socket, "self: ", "Error while device connecting: ", err.Error())
				return
			}
			var task = parsedMessage.Body
			var product = task.Product
			var payments = task.Payments
			// Log in operator
			device.SetParam(1021, task.Operator);

			if err = device.OperatorLogin(); err != nil {
				errorHandle(socket, "self: ", "Error while operator login: ", err.Error());
				device.Close();
				return
			}

			// checking Shift state, close if expired

			writeToLog("self: ", "1. Checking shift state ")

			device.SetParam(fptr10.LIBFPTR_PARAM_DATA_TYPE, fptr10.LIBFPTR_DT_SHIFT_STATE)
			device.QueryData()

			shiftState := device.GetParamInt(fptr10.LIBFPTR_PARAM_SHIFT_STATE)

			if shiftState == fptr10.LIBFPTR_SS_EXPIRED {
				writeToLog("self: ", "1.1 Closing expired Shift")
				device.SetParam(fptr10.LIBFPTR_PARAM_REPORT_TYPE, fptr10.LIBFPTR_RT_CLOSE_SHIFT)

				if err = device.Report(); err != nil {
					errorHandle(socket, "self: ", "Error while shift closing: ", err.Error())
					device.Close()
					return
				}

				if err = device.CheckDocumentClosed(); err != nil {
					errorHandle(socket, "self: ", "Error while document closing check: ", err.Error())
					device.Close()
					return
				}
			}

			writeToLog("self: ", "2. Try to open receipt")

			var side int
			switch task.Side {
			case Debit:
				side = fptr10.LIBFPTR_RT_BUY
			case Credit:
				side = fptr10.LIBFPTR_RT_SELL
			default:
				errorHandle(socket, "self: ", "Error while receipt type defining: ", "wrong receipt type param")
				device.Close()
				return
			}

			device.SetParam(fptr10.LIBFPTR_PARAM_RECEIPT_TYPE, side)

			if err = device.OpenReceipt(); err != nil {
				errorHandle(socket, "self: ", "Error while receipt opening: ", err.Error())
				device.Close()
				return
			}

			writeToLog("self: ", "Receipt has been opened successfully. ")
			writeToLog("self: ", "3. Try to register receipt ")

			device.SetParam(fptr10.LIBFPTR_PARAM_COMMODITY_NAME, product.Name)
			device.SetParam(fptr10.LIBFPTR_PARAM_PRICE, product.Price)
			device.SetParam(fptr10.LIBFPTR_PARAM_QUANTITY, product.Quantity)
			device.SetParam(fptr10.LIBFPTR_PARAM_TAX_TYPE, fptr10.LIBFPTR_TAX_NO)
			device.SetParam(1212, 4) // unnamed param responsible for the print line "service" or "product"

			if err = device.Registration(); err != nil {
				errorHandle(socket, "self: ", "Error while receipt registration: ", err.Error())
				device.Close()
				return
			}

			writeToLog("self: ", "Receipt has been registered successfully ")
			writeToLog("self: ", "4. Try to handle payments")

			for i := 0; i < len(payments); i++ {
				var paymentType int
				switch payments[i].Type {
				case "cash":
					paymentType = fptr10.LIBFPTR_PT_CASH
				case "electronically":
					paymentType = fptr10.LIBFPTR_PT_ELECTRONICALLY
				default:
					errorHandle(socket, "self: ", "Error while payment type defining: ", "invalid payment type param")
					device.Close()
					return
				}
				device.SetParam(fptr10.LIBFPTR_PARAM_PAYMENT_TYPE, paymentType)
				device.SetParam(fptr10.LIBFPTR_PARAM_PAYMENT_SUM, payments[i].Sum)
				if err = device.Payment(); err != nil {
					errorHandle(socket, "self: ", "Error while payment handling: ", err.Error())
					device.Close()
					return
				}
			}
			writeToLog("self: ", "Payments have been added")
			writeToLog("self: ", "5. Try to close receipt")

			device.CloseReceipt()

			if err = device.CheckDocumentClosed(); err != nil {
				errorHandle(socket, "self: ", "Error while document closing check: ", err.Error())
				device.Close()
				return
			}

			if !device.GetParamBool(fptr10.LIBFPTR_PARAM_DOCUMENT_CLOSED) {
				device.CancelReceipt()
				errorHandle(socket, "self: ", "Document wasn't closed. Receipt has been canceled: ", device.ErrorDescription())
				device.Close()
				return
			}

			if !device.GetParamBool(fptr10.LIBFPTR_PARAM_DOCUMENT_PRINTED) {
				errorHandle(socket, "self: ", "Error while print retry: ", device.ErrorDescription())
				device.Close()
				return
			}

			if err := device.Close(); err != nil {
				log.Println(err)
				return
			}

			writeToLog("self: ", "Everything seems to be OK")

			successJSON, _ := json.Marshal(map[string]string{"type": "success", "message": "We've reached 'ready' status"})
			socket.SendText(string(successJSON))

			writeToLog("self: ", " ======= Task finished ======== ")

		}
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

func main() {
	svcConfig := &service.Config{
		Name:        "AtolPrintUtility",
		DisplayName: "ATOL print service",
		Description: "Service for communication between atol device and remote nodejs application",
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

func connectDevice(port string) (*fptr10.IFptr, error) {

	fptr := fptr10.New()

	if fptr == nil {
		return fptr, errors.New("Cannot load driver")
	}

	settings := fmt.Sprintf("{ \"%v\": %d, \"%v\": %d, \"%v\": %s, \"%v\": %d }",
		fptr10.LIBFPTR_SETTING_MODEL, fptr10.LIBFPTR_MODEL_ATOL_AUTO,
		fptr10.LIBFPTR_SETTING_PORT, fptr10.LIBFPTR_PORT_COM,
		fptr10.LIBFPTR_SETTING_COM_FILE, port,
		fptr10.LIBFPTR_SETTING_BAUDRATE, fptr10.LIBFPTR_PORT_BR_115200)
	fptr.SetSettings(settings)

	for i := 1; i < CONNECTION_ATTEMPTS; i++ {
		fptr.Open()
		isOpened := fptr.IsOpened()
		if isOpened {
			return fptr, nil
		}
		time.Sleep(time.Duration(math.Pow(2, float64(i))) * time.Millisecond)
	}
	return fptr, fmt.Errorf("Cannot open connection for device on port %v", port)
}

func definePorts() (string, string) {
	fptr := fptr10.New()

	if fptr == nil {
		log.Fatal(errors.New("Cannot load driver"))
	}

	var pawnshopPort, leasingPort string

	for i := 1; i <= 9; i++ {
		var ii = strconv.Itoa(i)
		settings := fmt.Sprintf("{ \"%v\": %d, \"%v\": %d, \"%v\": %s, \"%v\": %d }",
			fptr10.LIBFPTR_SETTING_MODEL, fptr10.LIBFPTR_MODEL_ATOL_AUTO,
			fptr10.LIBFPTR_SETTING_PORT, fptr10.LIBFPTR_PORT_COM,
			fptr10.LIBFPTR_SETTING_COM_FILE, ii,
			fptr10.LIBFPTR_SETTING_BAUDRATE, fptr10.LIBFPTR_PORT_BR_115200)
		fptr.SetSettings(settings)

		if err := fptr.Open(); err != nil {
			writeToLog("self: ", "REJECTED COM-FILE: ", i)
			continue
		} else {
			fptr.SetParam(fptr10.LIBFPTR_PARAM_DATA_TYPE, fptr10.LIBFPTR_DT_SERIAL_NUMBER)
			fptr.QueryData()
			serialNumber := fptr.GetParamString(fptr10.LIBFPTR_PARAM_SERIAL_NUMBER)
			writeToLog("self: ", "S/N: ", serialNumber, " COM-FILE: ", ii)
			if serialNumber == PAWNSHOP_SERIAL {
				pawnshopPort = ii
				fptr.Close()
				continue
			} else if serialNumber == LEASING_SERIAL {
				leasingPort = ii
				fptr.Close()
				continue
			} else {
				writeToLog("self: ", "Unknown device with serial number: ", serialNumber)
				log.Fatal(errors.New("Unknown device"))
			}
		}
	}
	return pawnshopPort, leasingPort
}
