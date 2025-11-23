package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// ---------- Database Constants ----------
const (
	DB_USER     = "msg_admin"
	DB_PASSWORD = "54321"
	DB_NAME     = "message_app"
	DB_HOST     = "localhost"
	DB_PORT     = "5432"
)

// ---------- Global Variables ----------
var (
	db        *sql.DB
	jwtSecret = []byte("MOHAMMED_AYAAN_PASHA")
	hub       *Hub
)

// WebSocket upgrader configuration
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Allow all origins for development - restrict in production
		return true
	},
}

// ---------- Data Models ----------
type User struct {
	Email    string `json:"email"`
	UserName string `json:"username"`
	Password string `json:"password"`
}

type SearchResult struct {
	ID       string `json:"id"`
	UserName string `json:"username"`
}

// WebSocket Models
type wsIncoming struct {
	To      string `json:"to"`      // receiver user_id (UUID string)
	Message string `json:"message"` // the message text
}

type wsOutgoing struct {
	From      string    `json:"from"`
	Message   string    `json:"message"`
	MessageID string    `json:"message_id"`
	Delivered bool      `json:"delivered"`
	CreatedAt time.Time `json:"created_at"`
}

// ---------- WebSocket Client & Hub ----------
type Client struct {
	UserID string
	Conn   *websocket.Conn
	Send   chan []byte
}

type Hub struct {
	clients    map[string]*Client // user_id -> client
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			h.clients[c.UserID] = c
			h.mu.Unlock()
			log.Printf("user %s connected (total %d)\n", c.UserID, len(h.clients))
		case c := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[c.UserID]; ok {
				delete(h.clients, c.UserID)
				close(c.Send)
				log.Printf("user %s disconnected (total %d)\n", c.UserID, len(h.clients))
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) GetClient(userID string) (*Client, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.clients[userID]
	return c, ok
}

// ---------- WebSocket Helpers ----------
func insertMessage(senderID, receiverID, content string, delivered bool) (string, time.Time, error) {
	var id string
	var createdAt time.Time
	err := db.QueryRow(
		`INSERT INTO messages (sender_id, receiver_id, content, delivered)
		 VALUES ($1,$2,$3,$4) RETURNING id, created_at`,
		senderID, receiverID, content, delivered,
	).Scan(&id, &createdAt)
	return id, createdAt, err
}

// fetchUndeliveredMessages retrieves all undelivered messages for a user
func fetchUndeliveredMessages(receiverID string) ([]wsOutgoing, error) {
	rows, err := db.Query(`
		SELECT id, sender_id, content, created_at
		FROM messages
		WHERE receiver_id = $1 AND delivered = false
		ORDER BY created_at ASC
	`, receiverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []wsOutgoing
	for rows.Next() {
		var msg wsOutgoing
		err := rows.Scan(&msg.MessageID, &msg.From, &msg.Message, &msg.CreatedAt)
		if err != nil {
			return nil, err
		}
		msg.Delivered = false // will be marked true after sending
		messages = append(messages, msg)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return messages, nil
}

// markMessageAsDelivered updates the delivered status of a message
func markMessageAsDelivered(messageID string) error {
	_, err := db.Exec(`
		UPDATE messages
		SET delivered = true
		WHERE id = $1
	`, messageID)
	return err
}

// deliverOfflineMessages fetches and sends all undelivered messages to a client
func deliverOfflineMessages(client *Client) {
	messages, err := fetchUndeliveredMessages(client.UserID)
	if err != nil {
		log.Printf("Error fetching undelivered messages for user %s: %v", client.UserID, err)
		return
	}

	if len(messages) == 0 {
		log.Printf("No undelivered messages for user %s", client.UserID)
		return
	}

	log.Printf("Delivering %d offline messages to user %s", len(messages), client.UserID)

	for _, msg := range messages {
		// Mark as delivered before sending
		msg.Delivered = true
		encoded, err := json.Marshal(msg)
		if err != nil {
			log.Printf("Error encoding message %s: %v", msg.MessageID, err)
			continue
		}

		// Try to send to client
		select {
		case client.Send <- encoded:
			// Successfully queued, now mark as delivered in DB
			if err := markMessageAsDelivered(msg.MessageID); err != nil {
				log.Printf("Error marking message %s as delivered: %v", msg.MessageID, err)
			} else {
				log.Printf("Delivered offline message %s to user %s", msg.MessageID, client.UserID)
			}
		default:
			// Channel full, skip this message for now
			log.Printf("Client %s send channel full, skipping message %s", client.UserID, msg.MessageID)
		}
	}
}

// ---------- WebSocket Handler ----------
func wsHandler(h *Hub) echo.HandlerFunc {
	return func(c echo.Context) error {
		// Extract token from query param
		tokenStr := c.QueryParam("token")
		if tokenStr == "" {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "missing token"})
		}

		// Parse and validate token
		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid token claims"})
		}
		userIDraw, ok := claims["user_id"]
		if !ok {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "token missing user_id"})
		}
		userID, ok := userIDraw.(string)
		if !ok {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "user_id claim invalid"})
		}

		// Upgrade to WebSocket connection
		wsConn, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			log.Println("upgrade error:", err)
			return err
		}

		client := &Client{
			UserID: userID,
			Conn:   wsConn,
			Send:   make(chan []byte, 32),
		}

		h.register <- client

		// Deliver any offline messages
		go deliverOfflineMessages(client)

		// Start writer and reader
		go writePump(client, h)
		readPump(client, h)

		return nil
	}
}

// readPump reads incoming messages from client
func readPump(client *Client, h *Hub) {
	defer func() {
		h.unregister <- client
		client.Conn.Close()
	}()

	client.Conn.SetReadLimit(1024 * 1024)
	client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	client.Conn.SetPongHandler(func(string) error {
		client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, raw, err := client.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Println("read error:", err)
			}
			break
		}

		var inc wsIncoming
		if err := json.Unmarshal(raw, &inc); err != nil {
			log.Println("invalid payload:", err)
			continue
		}
		if inc.To == "" || inc.Message == "" {
			// ignore invalid
			continue
		}

		// Check if recipient connected
		recipientClient, connected := h.GetClient(inc.To)
		delivered := false
		var msgID string
		var createdAt time.Time
		if connected {
			// Build outgoing message
			out := wsOutgoing{
				From:    client.UserID,
				Message: inc.Message,
			}
			// persist with delivered = true
			msgID, createdAt, err = insertMessage(client.UserID, inc.To, inc.Message, true)
			if err != nil {
				log.Println("db insert error (delivered):", err)
			} else {
				out.MessageID = msgID
				out.CreatedAt = createdAt
				out.Delivered = true
			}

			encoded, _ := json.Marshal(out)

			// Send to recipient
			select {
			case recipientClient.Send <- encoded:
				delivered = true
			default:
				delivered = false
			}
		} else {
			// recipient offline -> persist delivered=false
			msgID, createdAt, err = insertMessage(client.UserID, inc.To, inc.Message, false)
			if err != nil {
				log.Println("db insert error (undelivered):", err)
			}
		}

		// ACK back to sender
		ack := wsOutgoing{
			From:      client.UserID,
			Message:   inc.Message,
			MessageID: msgID,
			Delivered: delivered,
			CreatedAt: createdAt,
		}
		ackBytes, _ := json.Marshal(ack)

		// Send ACK to sender
		select {
		case client.Send <- ackBytes:
		default:
			// if sender channel blocked, drop the ack
		}
	}
}

// writePump writes queued messages from server to the client
func writePump(client *Client, h *Hub) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		client.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-client.Send:
			client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// hub closed the channel
				client.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			w, err := client.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(msg)
			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			// send ping
			client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := client.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ---------- Existing Application Logic ----------
func SearchUsers(db *sql.DB, query string, excludeID string) ([]SearchResult, error) {
	rows, err := db.Query(`
		SELECT id, user_name,
	       similarity(user_name, $1) AS score
		FROM users
		WHERE similarity(user_name, $1) > 0.2
	  		AND id <> $2
		ORDER BY score DESC
		LIMIT 20
	`, query, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := []SearchResult{}
	for rows.Next() {
		var u SearchResult
		if err := rows.Scan(&u.ID, &u.UserName); err != nil {
			return nil, err
		}
		results = append(results, u)
	}
	return results, nil
}

func main() {
	var err error

	// Initialize database connection
	psqlInfo := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		DB_USER, DB_PASSWORD, DB_HOST, DB_PORT, DB_NAME)

	db, err = sql.Open("postgres", psqlInfo)
	if err != nil {
		log.Fatal("Error connecting to database:", err)
	}
	defer db.Close()

	// Verify connection
	err = db.Ping()
	if err != nil {
		log.Fatal("Cannot reach database:", err)
	}

	fmt.Println("✅ Connected to database")

	// Initialize WebSocket hub
	hub = NewHub()
	go hub.Run()

	// Create Echo instance
	e := echo.New()

	// Public routes
	e.POST("/signup", signupHandler)
	e.POST("/login", loginHandler)

	// JWT protected routes
	api := e.Group("/api")
	api.Use(echojwt.WithConfig(echojwt.Config{
		SigningKey: jwtSecret,
	}))
	api.GET("/search", searchHandler)

	// WebSocket route
	e.GET("/ws", wsHandler(hub))

	fmt.Println("🚀 Server running on http://localhost:8080")
	fmt.Println("📡 WebSocket endpoint available at ws://localhost:8080/ws?token=JWT_TOKEN")
	e.Logger.Fatal(e.Start(":8080"))
}

func loginHandler(c echo.Context) error {
	var creds struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	if err := c.Bind(&creds); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
	}
	if creds.Email == "" || creds.Password == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Email and password are required"})
	}

	// Fetch stored user
	var userID string
	var hashedPassword string
	err := db.QueryRow(`SELECT id, password_hash FROM users WHERE email=$1`, creds.Email).
		Scan(&userID, &hashedPassword)
	if err == sql.ErrNoRows {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Invalid email or password"})
	}
	if err != nil {
		log.Println("DB query error:", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database error"})
	}

	// Verify password
	if bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(creds.Password)) != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Invalid email or password"})
	}

	// Generate tokens
	accessToken, err := generateJWT(userID, 30*time.Minute)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create access token"})
	}

	refreshToken, err := generateRefreshToken()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create refresh token"})
	}

	expiry := time.Now().Add(7 * 24 * time.Hour)

	// Optional: delete old refresh tokens for same user
	_, _ = db.Exec(`DELETE FROM refresh_tokens WHERE user_id=$1`, userID)

	// Store new refresh token
	_, err = db.Exec(`
		INSERT INTO refresh_tokens (user_id, token, expires_at)
		VALUES ($1, $2, $3)
	`, userID, refreshToken, expiry)
	if err != nil {
		log.Println("Insert refresh token error:", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to store refresh token"})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"message":       "Login successful",
		"access_token":  accessToken,
		"refresh_token": refreshToken,
	})
}

func searchHandler(c echo.Context) error {
	// Extract claims from JWT
	user := c.Get("user").(*jwt.Token)
	claims := user.Claims.(jwt.MapClaims)
	userID := claims["user_id"].(string)

	searchQuery := c.QueryParam("username")
	if searchQuery == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "username query parameter required"})
	}

	results, err := SearchUsers(db, searchQuery, userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database query failed"})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"results": results,
	})
}

func signupHandler(c echo.Context) error {
	u := new(User)
	if err := c.Bind(u); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
	}

	if u.Email == "" || u.UserName == "" || u.Password == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "All fields are required"})
	}

	// Check if email exists
	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE email=$1)", u.Email).Scan(&exists)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database error"})
	}
	if exists {
		return c.JSON(http.StatusConflict, map[string]string{"error": "Email already registered"})
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Password hashing failed"})
	}

	// Insert new user and return their UUID
	var userID string
	err = db.QueryRow(`
		INSERT INTO users (email, user_name, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id
	`, u.Email, u.UserName, string(hashedPassword)).Scan(&userID)
	if err != nil {
		log.Println("Insert error:", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create user"})
	}

	// Generate access token (valid 30 min)
	accessToken, err := generateJWT(userID, 30*time.Minute)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create access token"})
	}

	// Generate refresh token (valid 7 days)
	refreshToken, err := generateRefreshToken()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create refresh token"})
	}

	expiry := time.Now().Add(7 * 24 * time.Hour)
	_, err = db.Exec(`
		INSERT INTO refresh_tokens (user_id, token, expires_at)
		VALUES ($1, $2, $3)
	`, userID, refreshToken, expiry)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to store refresh token"})
	}

	return c.JSON(http.StatusCreated, map[string]interface{}{
		"message":       "User registered successfully",
		"access_token":  accessToken,
		"refresh_token": refreshToken,
	})
}

func generateJWT(userID string, duration time.Duration) (string, error) {
	claims := jwt.MapClaims{
		"user_id": userID,
		"exp":     time.Now().Add(duration).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

func generateRefreshToken() (string, error) {
	bytes := make([]byte, 32)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(bytes), nil
}
