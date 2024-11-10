package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Message struct {
	Sender  string `json:"sender"`
	Content string `json:"content"`
}

type Client struct {
	conn *websocket.Conn
	send chan []byte
	room *Room
	name string
}

type Room struct {
	name       string
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mutex      sync.RWMutex
}

type Hub struct {
	rooms map[string]*Room
	mutex sync.RWMutex
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Generate a random animal name for users
func generateName() string {
	adjectives := []string{"Happy", "Sleepy", "Jumpy", "Grumpy", "Silly", "Friendly"}
	animals := []string{"Penguin", "Giraffe", "Elephant", "Kangaroo", "Dolphin", "Koala"}

	// Seed the random number generator to ensure different results each time
	rand.Seed(time.Now().UnixNano())

	// Select random adjective, animal, and a random number between 1 and 99
	adjective := adjectives[rand.Intn(len(adjectives))]
	animal := animals[rand.Intn(len(animals))]
	number := rand.Intn(99) + 1 // Ensures we get a number between 1 and 99

	return fmt.Sprintf("%s%s%d", adjective, animal, number)
}

func newHub() *Hub {
	return &Hub{
		rooms: make(map[string]*Room),
	}
}

func newRoom(name string) *Room {
	return &Room{
		name:       name,
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) getOrCreateRoom(name string) *Room {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if room, exists := h.rooms[name]; exists {
		return room
	}

	room := newRoom(name)
	h.rooms[name] = room
	go room.run()
	return room
}

func (r *Room) run() {
	for {
		select {
		case client := <-r.register:
			// Register client and notify all clients
			r.mutex.Lock()
			r.clients[client] = true
			r.mutex.Unlock()

			// Notify everyone in the room about the new client joining
			joinMsg := Message{
				Sender:  "System",
				Content: fmt.Sprintf("%s joined the room", client.name),
			}
			r.broadcastMessage(joinMsg)

		case client := <-r.unregister:
			// Remove client and notify all clients
			r.mutex.Lock()
			if _, ok := r.clients[client]; ok {
				delete(r.clients, client)
				close(client.send)
			}
			r.mutex.Unlock()

			// Notify everyone in the room about the client leaving
			leaveMsg := Message{
				Sender:  "System",
				Content: fmt.Sprintf("%s left the room", client.name),
			}
			r.broadcastMessage(leaveMsg)

		case message := <-r.broadcast:
			// Broadcast received message to all clients
			r.broadcastToAll(message)
		}
	}
}

// Helper function to broadcast a structured message to all clients
func (r *Room) broadcastMessage(msg Message) {
	msgBytes, _ := json.Marshal(msg)
	r.broadcastToAll(msgBytes)
}

// Helper function to broadcast raw message bytes to all clients
func (r *Room) broadcastToAll(message []byte) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	for client := range r.clients {
		select {
		case client.send <- message:
		default:
			// If send channel is full, close it and remove the client
			close(client.send)
			delete(r.clients, client)
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		log.Printf("readPump ending for client: %s", c.name)
		c.room.unregister <- c
		c.conn.Close()
	}()

	for {
		_, rawMessage, err := c.conn.ReadMessage()
		if err != nil {
			log.Printf("Error reading message from client %s: %v", c.name, err)
			break
		}

		log.Printf("Received raw message from %s: %s", c.name, string(rawMessage))

		msg := Message{
			Sender:  c.name,
			Content: string(rawMessage),
		}

		msgBytes, err := json.Marshal(msg)
		if err != nil {
			log.Printf("Error marshaling message: %v", err)
			continue
		}

		log.Printf("Broadcasting message from %s: %s", c.name, string(msgBytes))
		c.room.broadcast <- msgBytes
	}
}

func (c *Client) writePump() {
	defer func() {
		log.Printf("writePump ending for client: %s", c.name)
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				log.Printf("Client %s send channel closed", c.name)
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			log.Printf("writePump received message for client %s: %s", c.name, string(message))
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				log.Printf("Error getting writer for client %s: %v", c.name, err)
				return
			}

			_, err = w.Write(message)
			if err != nil {
				log.Printf("Error writing message for client %s: %v", c.name, err)
				return
			}

			if err := w.Close(); err != nil {
				log.Printf("Error closing writer for client %s: %v", c.name, err)
				return
			}
			log.Printf("Successfully wrote message to websocket for client %s", c.name)
		}
	}
}

func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	roomName := r.URL.Query().Get("room")
	if roomName == "" {
		http.Error(w, "Room name is required", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	log.Printf("getting or creating room %s", roomName)
	room := hub.getOrCreateRoom(roomName)
	client := &Client{
		conn: conn,
		send: make(chan []byte, 256),
		room: room,
		name: generateName(),
	}

	go client.writePump()

	room.register <- client

	go client.readPump()
}

func main() {
	hub := newHub()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
