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

// Message struct represents a chat message with a sender and content.
type Message struct {
	Sender  string `json:"sender"`
	Content string `json:"content"`
}

// Client struct represents a single client connection.
type Client struct {
	conn *websocket.Conn // WebSocket connection
	send chan []byte     // Channel for outbound messages
	room *Room           // Room client is part of
	name string          // Name assigned to the client
}

// Room struct represents a chat room.
type Room struct {
	name       string           // Room name
	clients    map[*Client]bool // Set of connected clients
	broadcast  chan []byte      // Channel for broadcasting messages
	register   chan *Client     // Channel for registering new clients
	unregister chan *Client     // Channel for unregistering clients
	mutex      sync.RWMutex     // Mutex for synchronizing access to clients
}

// Hub struct holds a collection of chat rooms.
type Hub struct {
	rooms map[string]*Room // Map of room names to room instances
	mutex sync.RWMutex     // Mutex for synchronizing access to rooms
}

// WebSocket upgrader settings to accept all connections
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Accept all origins
	},
}

// generateName creates a random name by combining an adjective, animal, and number.
func generateName() string {
	adjectives := []string{"Happy", "Sleepy", "Jumpy", "Grumpy", "Silly", "Friendly"}
	animals := []string{"Penguin", "Giraffe", "Elephant", "Kangaroo", "Dolphin", "Koala"}

	// Seed the random generator and choose random items from lists
	rand.Seed(time.Now().UnixNano())
	adjective := adjectives[rand.Intn(len(adjectives))]
	animal := animals[rand.Intn(len(animals))]
	number := rand.Intn(99) + 1 // Number between 1 and 99

	return fmt.Sprintf("%s%s%d", adjective, animal, number)
}

// newHub initializes a Hub with an empty room map.
func newHub() *Hub {
	return &Hub{
		rooms: make(map[string]*Room),
	}
}

// newRoom creates a new Room with initialized channels and client set.
func newRoom(name string) *Room {
	return &Room{
		name:       name,
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// getOrCreateRoom retrieves a room by name or creates it if it doesn’t exist.
func (h *Hub) getOrCreateRoom(name string) *Room {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	// Return existing room if it exists, otherwise create and start it
	if room, exists := h.rooms[name]; exists {
		return room
	}

	room := newRoom(name)
	h.rooms[name] = room
	go room.run()
	return room
}

// run starts the main loop for a Room, handling register, unregister, and broadcast events.
func (r *Room) run() {
	for {
		select {
		case client := <-r.register:
			// Register client and notify all clients in the room
			r.mutex.Lock()
			r.clients[client] = true
			r.mutex.Unlock()
			joinMsg := Message{Sender: "System", Content: fmt.Sprintf("%s joined the room", client.name)}
			r.broadcastMessage(joinMsg)

		case client := <-r.unregister:
			// Unregister client and notify all clients in the room
			r.mutex.Lock()
			if _, ok := r.clients[client]; ok {
				delete(r.clients, client)
				close(client.send) // Close client's send channel
			}
			r.mutex.Unlock()
			leaveMsg := Message{Sender: "System", Content: fmt.Sprintf("%s left the room", client.name)}
			r.broadcastMessage(leaveMsg)

		case message := <-r.broadcast:
			// Send broadcast message to all clients in the room
			r.broadcastToAll(message)
		}
	}
}

// broadcastMessage encodes and sends a Message struct to all clients.
func (r *Room) broadcastMessage(msg Message) {
	msgBytes, _ := json.Marshal(msg)
	r.broadcastToAll(msgBytes)
}

// broadcastToAll sends raw message bytes to each client’s send channel.
func (r *Room) broadcastToAll(message []byte) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	for client := range r.clients {
		select {
		case client.send <- message:
		default:
			// If send channel is full, close it and remove client
			close(client.send)
			delete(r.clients, client)
		}
	}
}

// readPump reads messages from the WebSocket connection, broadcasting them to the room.
func (c *Client) readPump() {
	defer func() {
		log.Printf("readPump ending for client: %s", c.name)
		c.room.unregister <- c // Unregister client on disconnect
		c.conn.Close()
	}()

	for {
		_, rawMessage, err := c.conn.ReadMessage()
		if err != nil {
			log.Printf("Error reading message from client %s: %v", c.name, err)
			break
		}

		msg := Message{Sender: c.name, Content: string(rawMessage)}
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			log.Printf("Error marshaling message: %v", err)
			continue
		}

		c.room.broadcast <- msgBytes // Broadcast message to room
	}
}

// writePump reads messages from the send channel and writes them to the WebSocket connection.
func (c *Client) writePump() {
	defer func() {
		log.Printf("writePump ending for client: %s", c.name)
		c.conn.Close()
	}()

	for message := range c.send {
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
	}
}

// serveWs handles incoming WebSocket connections, creating and managing a Client for each.
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

	room := hub.getOrCreateRoom(roomName)
	client := &Client{
		conn: conn,
		send: make(chan []byte, 256),
		room: room,
		name: generateName(),
	}

	go client.writePump() // Start writePump in separate goroutine

	room.register <- client // Register client with the room

	go client.readPump() // Start readPump in separate goroutine
}

// main sets up the WebSocket server on port 8080 and starts it.
func main() {
	hub := newHub()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
