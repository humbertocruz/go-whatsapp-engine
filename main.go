package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Configuração
var (
	WebhookURL    = getEnv("WEBHOOK_URL", "http://localhost:3000/api/webhook/whatsapp")
	EnginePort    = getEnv("PORT", "3002")
	AllowedSender = getEnv("ALLOWED_SENDER", "556199836903,5561992178060") // Beto (Pessoal + Teste)
)

type WhatsAppInstance struct {
	Client *whatsmeow.Client
	ID     string
	QR     string
	Status string // DISCONNECTED, CONNECTING, CONNECTED
}

var (
	instances = make(map[string]*WhatsAppInstance)
	instMutex sync.RWMutex
	container *sqlstore.Container
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func main() {
	dbLog := waLog.Stdout("Database", "DEBUG", true)
	var err error
	container, err = sqlstore.New(context.Background(), "sqlite3", "file:session.db?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}

	go startAPI()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

func forwardToWebhook(instanceID string, eventType string, data interface{}) {
	payload := map[string]interface{}{
		"instanceId": instanceID,
		"event":      eventType,
		"data":       data,
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(WebhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("❌ [%s] Erro Webhook: %v\n", instanceID, err)
		return
	}
	defer resp.Body.Close()
}

func registerHandler(inst *WhatsAppInstance) {
	inst.Client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			if !v.Info.IsFromMe {
				sender := v.Info.Sender.User
				// Filtro de Segurança: Em DEV/Teste, só responde ao Beto para evitar spam
				if !strings.Contains(AllowedSender, sender) {
					fmt.Printf("🚫 [%s] Mensagem de %s ignorada (Filtro de Segurança Ativo)\n", inst.ID, sender)
					return
				}

				fmt.Printf("📩 [%s] Mensagem de %s\n", inst.ID, v.Info.Sender.String())
				forwardToWebhook(inst.ID, "message", v)
			}
		case *events.Connected:
			inst.Status = "CONNECTED"
			inst.QR = ""
			forwardToWebhook(inst.ID, "status", map[string]string{"status": "CONNECTED"})
			fmt.Printf("✅ [%s] Conectado!\n", inst.ID)
		case *events.LoggedOut:
			inst.Status = "DISCONNECTED"
			forwardToWebhook(inst.ID, "status", map[string]string{"status": "DISCONNECTED"})
			fmt.Printf("🚪 [%s] Deslogado!\n", inst.ID)
		}
	})
}

func startAPI() {
	r := gin.Default()
	r.Use(cors.Default())

	r.GET("/instances", func(c *gin.Context) {
		instMutex.RLock()
		defer instMutex.RUnlock()
		
		response := make(map[string]interface{})
		for id, inst := range instances {
			response[id] = map[string]string{
				"id":     inst.ID,
				"status": inst.Status,
				"qr":     inst.QR,
			}
		}
		c.JSON(200, response)
	})

	r.POST("/instances/:id/connect", func(c *gin.Context) {
		id := c.Param("id")
		
		instMutex.Lock()
		if inst, exists := instances[id]; exists && inst.Status == "CONNECTED" {
			instMutex.Unlock()
			c.JSON(400, gin.H{"error": "Já conectado"})
			return
		}

		deviceStore, err := container.GetDevice(context.Background(), types.JID{User: id, Server: types.DefaultUserServer})
		if err != nil || deviceStore == nil {
			deviceStore = container.NewDevice()
		}

		client := whatsmeow.NewClient(deviceStore, waLog.Stdout("Client-"+id, "INFO", true))
		inst := &WhatsAppInstance{
			Client: client,
			ID:     id,
			Status: "CONNECTING",
		}
		instances[id] = inst
		instMutex.Unlock()

		registerHandler(inst)

		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			c.JSON(500, gin.H{"error": "Erro ao conectar"})
			return
		}

		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					instMutex.Lock()
					inst.QR = evt.Code
					instMutex.Unlock()
					forwardToWebhook(inst.ID, "qr", map[string]string{"code": evt.Code})
				}
			}
		}()

		c.JSON(200, gin.H{"message": "Iniciando...", "status": "CONNECTING"})
	})

	r.POST("/instances/:id/send", func(c *gin.Context) {
		id := c.Param("id")
		var req struct {
			To   string `json:"to"`
			Text string `json:"text"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "Payload inválido"})
			return
		}

		instMutex.RLock()
		inst, exists := instances[id]
		instMutex.RUnlock()

		if !exists || inst.Status != "CONNECTED" {
			c.JSON(400, gin.H{"error": "Instância não conectada"})
			return
		}

		jid, _ := types.ParseJID(req.To)
		msg := &waE2E.Message{Conversation: proto.String(req.Text)}
		_, err := inst.Client.SendMessage(context.Background(), jid, msg)
		
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"success": true})
	})

	fmt.Printf("📡 Multi-Engine rodando na porta %s\n", EnginePort)
	r.Run(":" + EnginePort)
}
