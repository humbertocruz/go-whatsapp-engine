package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type WhatsAppInstance struct {
	Client     *whatsmeow.Client
	ID         string
	QR         string
	Status     string
	Disconnect func()
}

var (
	instances = make(map[string]*WhatsAppInstance)
	instMutex sync.RWMutex
	container *sqlstore.Container
)

func main() {
	dbLog := waLog.Stdout("Database", "DEBUG", true)
	var err error
	container, err = sqlstore.New(context.Background(), "sqlite3", "file:session.db?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}

	// Restaurar sessões antigas
	devices, err := container.GetAllDevices()
	if err == nil {
		for _, device := range devices {
			fmt.Printf("♻️ Restaurando instância: %s\n", device.ID.String())
			// Lógica de restauração simplificada
		}
	}

	go startAPI()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

func startAPI() {
	r := gin.Default()

	r.GET("/instances", func(c *gin.Context) {
		instMutex.RLock()
		defer instMutex.RUnlock()
		c.JSON(200, instances)
	})

	r.POST("/instances/:id/connect", func(c *gin.Context) {
		id := c.Param("id")
		
		instMutex.Lock()
		if _, exists := instances[id]; exists {
			instMutex.Unlock()
			c.JSON(400, gin.H{"error": "Instância já ativa"})
			return
		}

		deviceStore, err := container.GetDevice(types.JID{User: id, Server: types.DefaultUserServer})
		if err != nil {
			deviceStore = container.NewDevice()
		}

		client := whatsmeow.NewClient(deviceStore, waLog.Stdout("Client-"+id, "INFO", true))
		
		instance := &WhatsAppInstance{
			Client: client,
			ID:     id,
			Status: "CONNECTING",
		}
		instances[id] = instance
		instMutex.Unlock()

		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			c.JSON(500, gin.H{"error": "Falha ao conectar"})
			return
		}

		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					instance.QR = evt.Code
					fmt.Printf("📸 QR Code para %s: %s\n", id, evt.Code)
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				}
			}
		}()

		c.JSON(200, gin.H{"message": "Processo de conexão iniciado"})
	})

	r.Run(":3002")
}
