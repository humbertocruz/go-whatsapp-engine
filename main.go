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
	"syscall"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Configuração via Variáveis de Ambiente
var (
	WebhookURL    = getEnv("WEBHOOK_URL", "http://localhost:3000/api/webhook/whatsapp")
	EnginePort    = getEnv("PORT", "3002")
	AllowedSender = getEnv("ALLOWED_SENDER", "5561992178060,243860290682986")
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

var client *whatsmeow.Client
var allowedMap map[string]bool

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		sender := v.Info.Sender.User

		fmt.Printf("🔍 Tentativa de contato de: %s (ID: %s)\n", v.Info.Sender.String(), sender)

		// Filtro de Segurança
		if !allowedMap[sender] {
			fmt.Printf("🚫 Mensagem de %s ignorada (Filtro Ativo)\n", sender)
			return
		}

		fmt.Printf("📩 Mensagem autorizada de %s: %s\n", sender, v.Message.GetConversation())
		go forwardToWebhook(v)
	}
}

func forwardToWebhook(evt *events.Message) {
	payload := map[string]interface{}{
		"event": "message",
		"data": map[string]interface{}{
			"key": map[string]interface{}{
				"remoteJid": evt.Info.Sender.String(),
				"fromMe":    evt.Info.IsFromMe,
				"id":        evt.Info.ID,
			},
			"message":          evt.Message,
			"pushName":         evt.Info.PushName,
			"messageTimestamp": evt.Info.Timestamp.Unix(),
		},
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(WebhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("❌ Erro Webhook: %v\n", err)
		return
	}
	defer resp.Body.Close()
	fmt.Printf("✅ Webhook enviado: %s\n", resp.Status)
}

func startServer() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "alive"})
	})

	r.POST("/send", func(c *gin.Context) {
		var req struct {
			To   string `json:"to"`
			Text string `json:"text"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		jid, err := types.ParseJID(req.To)
		if err != nil {
			c.JSON(400, gin.H{"error": "Invalid JID"})
			return
		}

		msg := &waE2E.Message{Conversation: proto.String(req.Text)}
		_, err = client.SendMessage(context.Background(), jid, msg)
		if err != nil {
			c.JSON(500, gin.H{"success": false, "error": err.Error()})
			return
		}

		fmt.Printf("🚀 Resposta enviada para %s\n", req.To)
		c.JSON(200, gin.H{"success": true})
	})

	fmt.Printf("📡 Engine API na porta %s\n", EnginePort)
	r.Run(":" + EnginePort)
}

func main() {
	// Inicializar mapa de permitidos
	allowedMap = make(map[string]bool)
	for _, s := range strings.Split(AllowedSender, ",") {
		allowedMap[strings.TrimSpace(s)] = true
	}

	dbLog := waLog.Stdout("Database", "DEBUG", true)
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:session.db?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		panic(err)
	}

	client = whatsmeow.NewClient(deviceStore, waLog.Stdout("Client", "INFO", true))
	client.AddEventHandler(eventHandler)

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				fmt.Println("QR Code gerado! Escaneie no seu WhatsApp. 🌸")
			}
		}
	} else {
		err = client.Connect()
		if err != nil {
			panic(err)
		}
	}

	go startServer()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	client.Disconnect()
}
