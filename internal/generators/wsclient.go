package generators

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
)

type WSClient struct {
	conn *websocket.Conn
	url  string
	done chan struct{}
}

func NewWSClient(url string, auth string) (*WSClient, error) {

	headers := make(http.Header)
	headers.Add("Authorization", auth)

	conn, _, err := websocket.DefaultDialer.Dial(url, headers)
	if err != nil {
		return nil, err
	}

	client := &WSClient{
		conn: conn,
		url:  url,
		done: make(chan struct{}),
	}

	go client.listenMessages()

	return client, nil
}

func (c *WSClient) listenMessages() {
	defer close(c.done)
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Println("Error reading message:", err)
			return
		}
		log.Printf("Received: %s", message)
	}
}

func (c *WSClient) SendMessage(message string) error {
	err := c.conn.WriteMessage(websocket.TextMessage, []byte(message))
	if err != nil {
		log.Println("Error sending message:", err)
		return err
	}
	return nil
}

func (c *WSClient) ReadMessages() {
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Println("read:", err)
			return
		}
		log.Printf("recv: %s", message)
	}
}

func (c *WSClient) Close() error {
	err := c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		return err
	}
	select {
	case <-c.done:
	case <-time.After(time.Second):
	}
	return c.conn.Close()
}

// WaitForInterrupt waits for an interrupt signal to gracefully close the WebSocket connection.
func (c *WSClient) WaitForInterrupt() {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	select {
	case <-interrupt:
		log.Println("Interrupt received, closing connection...")
		c.Close()
	}
}
