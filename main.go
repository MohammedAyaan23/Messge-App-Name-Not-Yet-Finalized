package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// ---------- Database Constants ----------
var (
	DB_USER     = os.Getenv("DB_USER")
	DB_PASSWORD = os.Getenv("DB_PASSWORD")
	DB_NAME     = os.Getenv("DB_NAME")
	DB_HOST     = os.Getenv("DB_HOST")
	DB_PORT     = os.Getenv("DB_PORT")
)

// ---------- Global Variables ----------
var (
	db        *sql.DB
	jwtSecret = []byte("")
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
	TempID  string `json:"temp_id"`
}

type wsOutgoing struct {
	From         string    `json:"from"`
	FromUserName string    `json:"from_username"`
	Message      string    `json:"message"`
	MessageID    string    `json:"message_id"`
	Delivered    bool      `json:"delivered"`
	CreatedAt    time.Time `json:"created_at"`
	TempID       string    `json:"temp_id"`
}

// Conversation History Model
type ConversationMessage struct {
	MessageID      string    `json:"message_id"`
	SenderID       string    `json:"sender_id"`
	SenderUserName string    `json:"sender_username"`
	ReceiverID     string    `json:"receiver_id"`
	Content        string    `json:"content"`
	Delivered      bool      `json:"delivered"`
	CreatedAt      time.Time `json:"created_at"`
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

// getUserName fetches username by user ID
func getUserName(userID string) string {
	var username string
	err := db.QueryRow(`SELECT user_name FROM users WHERE id = $1`, userID).Scan(&username)
	if err != nil {
		log.Printf("Error fetching username for user %s: %v", userID, err)
		return "Unknown User"
	}
	return username
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
		msg.Delivered = false                    // will be marked true after sending
		msg.FromUserName = getUserName(msg.From) // Fetch sender's username
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
		log.Println("websocket handler hit")

		tokenStr := c.QueryParam("token")
		if tokenStr == "" {
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error": "missing access token",
			})
		}
		fmt.Println("tokenStr", tokenStr)

		//---------------------------------------------------------
		// Parse token using classic JWT API (v3/v4)
		//---------------------------------------------------------
		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method")
			}
			return jwtSecret, nil
		})
		fmt.Println("token", token)

		//---------------------------------------------------------
		// Detect expiration using ValidationError
		//---------------------------------------------------------
		var expired bool
		if err != nil {
			if ve, ok := err.(*jwt.ValidationError); ok {
				if ve.Errors&jwt.ValidationErrorExpired != 0 {
					expired = true
				}
			}
		}
		fmt.Println("expired", expired)

		//---------------------------------------------------------
		// 1) Access token valid → upgrade WS
		//---------------------------------------------------------
		if err == nil && token.Valid {
			fmt.Println("token valid")
			claims := token.Claims.(jwt.MapClaims)
			userID, ok := claims["user_id"].(string)
			if !ok {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error": "invalid token claims",
				})
			}

			wsConn, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
			if err != nil {
				fmt.Println("error upgrading to ws", err)
				return err
			}

			client := &Client{
				UserID: userID,
				Conn:   wsConn,
				Send:   make(chan []byte, 32),
			}

			h.register <- client
			go deliverOfflineMessages(client)
			go writePump(client, h)
			readPump(client, h)
			return nil
		}

		//---------------------------------------------------------
		// 2) Invalid but NOT expired → reject
		//---------------------------------------------------------
		if err != nil && !expired {
			fmt.Println("token invalid")
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error":  "invalid access token",
				"detail": err.Error(),
			})
		}

		//---------------------------------------------------------
		// 3) EXPIRED access token → try refresh token cookie
		//---------------------------------------------------------
		fmt.Println("checking token expiration")
		refreshCookie, err := c.Cookie("refresh_token")
		if err != nil {
			fmt.Println("refresh token missing")
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error":  "access_token_expired",
				"detail": "refresh token missing",
			})
		}

		refreshTok := refreshCookie.Value
		fmt.Println("refresh token", refreshTok)

		var dbUserID string
		var expiresAt time.Time

		err = db.QueryRow(`
			SELECT user_id, expires_at
			FROM refresh_tokens
			WHERE token = $1
		`, refreshTok).Scan(&dbUserID, &expiresAt)

		if err == sql.ErrNoRows {
			fmt.Println("refresh token invalid")
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error": "invalid refresh token",
			})
		}

		if time.Now().After(expiresAt) {
			fmt.Println("refresh token expired")
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error": "refresh token expired",
			})
		}

		//---------------------------------------------------------
		// 4) Refresh token valid → issue new access token
		//---------------------------------------------------------
		newAccess, err := generateJWT(dbUserID, 30*time.Minute)
		fmt.Println("new access token", newAccess)
		if err != nil {
			fmt.Println("failed to create new access token")
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"error": "failed to create new access token",
			})
		}

		//---------------------------------------------------------
		// 5) Tell client to retry WS connection with new token
		//---------------------------------------------------------
		return c.JSON(http.StatusOK, map[string]interface{}{
			"error":            "access_token_expired",
			"new_access_token": newAccess,
			"retry_ws":         true,
		})
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
		if inc.To == "" || inc.Message == "" || inc.TempID == "" {
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
				From:         client.UserID,
				FromUserName: getUserName(client.UserID),
				Message:      inc.Message,
				TempID:       inc.TempID,
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
			From:         client.UserID,
			FromUserName: getUserName(client.UserID),
			Message:      inc.Message,
			MessageID:    msgID,
			Delivered:    delivered,
			CreatedAt:    createdAt,
			TempID:       inc.TempID,
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
			fmt.Println("writing message to client")
			client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// hub closed the channel
				fmt.Println("inside !ok", ok)
				client.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			w, err := client.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				fmt.Println("inside err!=nil 1", err)
				return
			}
			w.Write(msg)
			if err := w.Close(); err != nil {
				fmt.Println("inside err!=nil 2", err)
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
		var score float64
		if err := rows.Scan(&u.ID, &u.UserName, &score); err != nil {
			return nil, err
		}
		results = append(results, u)
	}
	return results, nil
}

func JWTHandler(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		authHeader := c.Request().Header.Get("Authorization")
		if authHeader == "" {
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error": "missing_authorization_header",
			})
		}
		parts := strings.Split(authHeader, " ") // now split the header because a general header will look like this "Bearer <token>"
		if len(parts) != 2 || parts[0] != "Bearer" {
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error": "invalid_authorization_header",
			})
		}
		tokenStr := parts[1]

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return jwtSecret, nil
		})

		if err != nil {
			// token expiration
			if ve, ok := err.(*jwt.ValidationError); ok {
				if ve.Errors&jwt.ValidationErrorExpired != 0 {
					return c.JSON(http.StatusUnauthorized, map[string]interface{}{
						"error":  "access_token_expired",
						"detail": "Token has expired",
						"code":   "TOKEN_EXPIRED",
					})
				}
			}
			// invalid token
			return c.JSON(http.StatusUnauthorized, map[string]interface{}{
				"error":  "invalid_token",
				"detail": err.Error(),
			})

		}

		if !token.Valid {
			return c.JSON(http.StatusUnauthorized, map[string]string{
				"error": "invalid_token",
			})
		}

		c.Set("user", token)
		return next(c)
	}
}

func main() {
	var err error
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Fallback for local development
	}

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

	// frontendURL := os.Getenv("FRONTEND_URL")
	// if frontendURL == "" {
	// 	log.Fatal("FRONTEND_URL environment variable is not set")
	// }
	frontendURL := "https://nebula-chat-mxrl.onrender.com"
	// Create Echo instance
	e := echo.New()
	// Add CORS Middleware
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		// Allow your local dev environment AND your future Render frontend URL
		AllowOrigins: []string{frontendURL, "http://localhost:3000", "https://1ripmk0hlg82sctoh0udccymhld2u2uflf9q6h8r7t8jaypq8d-h861731785.scf.usercontent.goog", "https://nebula-chat-az8d.onrender.com"},
		AllowMethods: []string{http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodOptions},
		AllowHeaders: []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, echo.HeaderAuthorization},
		// Important for cookies (refresh tokens)
		AllowCredentials: true,
	}))

	// Public routes
	e.POST("/signup", signupHandler)
	e.POST("/login", loginHandler)
	e.POST("/refresh", refreshTokenHandler)

	// JWT protected routes
	api := e.Group("/api")
	api.Use(JWTHandler)
	api.GET("/search", searchHandler)
	api.GET("/conversation", conversationHistoryHandler)

	// WebSocket route
	e.GET("/ws", wsHandler(hub))

	e.GET("/health", func(c echo.Context) error {
		log.Println("health check api hit")
		if err := db.Ping(); err != nil {
			return c.String(http.StatusInternalServerError, "Database down")
		}
		return c.String(http.StatusOK, "OK")
	})

	fmt.Println("🚀 Server running on http://localhost:8080")
	fmt.Println("📡 WebSocket endpoint available at ws://localhost:8080/ws?token=JWT_TOKEN")
	e.Logger.Fatal(e.Start(":" + port))
}

func loginHandler(c echo.Context) error {
	log.Println("login api hit")
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

	// fetch user
	var userID string
	var hashedPassword string
	err := db.QueryRow(`SELECT id, password_hash FROM users WHERE email=$1`, creds.Email).
		Scan(&userID, &hashedPassword)

	if err == sql.ErrNoRows {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Invalid email or password"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database error"})
	}

	// password check
	if bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(creds.Password)) != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Invalid email or password"})
	}

	// generate access token
	accessToken, err := generateJWT(userID, 30*time.Minute)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create access token"})
	}

	// generate refresh token
	refreshToken, err := generateRefreshToken()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create refresh token"})
	}

	expiry := time.Now().Add(7 * 24 * time.Hour)

	// remove old refresh tokens
	_, _ = db.Exec(`DELETE FROM refresh_tokens WHERE user_id=$1`, userID)

	// insert refresh token
	_, err = db.Exec(`
		INSERT INTO refresh_tokens (user_id, token, expires_at)
		VALUES ($1, $2, $3)
	`, userID, refreshToken, expiry)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to store refresh token"})
	}

	// SET HTTPONLY COOKIE
	cookie := &http.Cookie{
		Name:     "refresh_token",
		Value:    refreshToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, // set true in production
		SameSite: http.SameSiteLaxMode,
		Expires:  expiry,
	}
	http.SetCookie(c.Response(), cookie)

	// only return access token
	return c.JSON(http.StatusOK, map[string]interface{}{
		"message":      "Login successful",
		"access_token": accessToken,
	})
}

func refreshTokenHandler(c echo.Context) error {
	log.Println("refresh token api hit")
	// read refresh token from cookie
	cookie, err := c.Cookie("refresh_token")
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Missing refresh token"})
	}

	refreshToken := cookie.Value

	// validate refresh token in DB
	var userID string
	var expiresAt time.Time

	err = db.QueryRow(`
		SELECT user_id, expires_at 
		FROM refresh_tokens 
		WHERE token = $1
	`, refreshToken).Scan(&userID, &expiresAt)

	if err == sql.ErrNoRows {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Invalid refresh token"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database error"})
	}

	if time.Now().After(expiresAt) {
		// cleanup expired
		_, _ = db.Exec(`DELETE FROM refresh_tokens WHERE token = $1`, refreshToken)
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Refresh token expired"})
	}

	// create new access token
	newAccessToken, err := generateJWT(userID, 30*time.Minute)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create access token"})
	}

	// rotate refresh token for security
	newRefreshToken, err := generateRefreshToken()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create refresh token"})
	}

	newExpiry := time.Now().Add(7 * 24 * time.Hour)

	// update DB
	_, err = db.Exec(`
		UPDATE refresh_tokens
		SET token = $1, expires_at = $2, updated_at = NOW()
		WHERE token = $3
	`, newRefreshToken, newExpiry, refreshToken)

	if err != nil {
		// deliver access token even if rotation fails
		return c.JSON(http.StatusOK, map[string]interface{}{
			"access_token": newAccessToken,
		})
	}

	// SET NEW COOKIE
	newCookie := &http.Cookie{
		Name:     "refresh_token",
		Value:    newRefreshToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, // true in production
		SameSite: http.SameSiteLaxMode,
		Expires:  newExpiry,
	}
	http.SetCookie(c.Response(), newCookie)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"access_token": newAccessToken,
	})
}

func searchHandler(c echo.Context) error {
	log.Println("search api hit")
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
		// --- MANDATORY DEBUG LINE ---
		// We MUST print the underlying database error before sending the 500
		log.Printf("Database Error in SearchUsers: %v", err) // <-- ADD THIS LINE
		// -----------------------------
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database query failed"})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"results": results,
	})
}

// conversationHistoryHandler fetches all conversations for the authenticated user
func conversationHistoryHandler(c echo.Context) error {
	log.Println("conversation history api hit")
	// Extract claims from JWT
	user := c.Get("user").(*jwt.Token)
	claims := user.Claims.(jwt.MapClaims)
	userID := claims["user_id"].(string)
	contactID := c.QueryParam("contact_id")
	var rows *sql.Rows
	var err error
	// Fetch all messages where user is either sender or receiver
	if contactID != "" {
		// Case 2: Fetch history for a SPECIFIC contact
		rows, err = db.Query(`
            SELECT id, sender_id, receiver_id, content, delivered, created_at
            FROM messages
            WHERE (sender_id = $1 AND receiver_id = $2) OR (sender_id = $2 AND receiver_id = $1)
            ORDER BY created_at ASC
        `, userID, contactID)
	} else {
		// Case 1: Fetch ALL history for the authenticated user
		rows, err = db.Query(`
            SELECT id, sender_id, receiver_id, content, delivered, created_at
            FROM messages
            WHERE sender_id = $1 OR receiver_id = $1
            ORDER BY created_at ASC
        `, userID)
	}

	if err != nil {
		log.Printf("Error fetching conversation history: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database query failed"})
	}
	defer rows.Close()

	var messages []ConversationMessage
	for rows.Next() {
		var msg ConversationMessage
		err := rows.Scan(&msg.MessageID, &msg.SenderID, &msg.ReceiverID, &msg.Content, &msg.Delivered, &msg.CreatedAt)
		if err != nil {
			log.Printf("Error scanning message row: %v", err)
			continue
		}
		// Fetch sender's username
		msg.SenderUserName = getUserName(msg.SenderID)
		messages = append(messages, msg)
	}

	if err = rows.Err(); err != nil {
		log.Printf("Error iterating rows: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to retrieve messages"})
	}

	// Return empty array if no messages found
	if messages == nil {
		messages = []ConversationMessage{}
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"messages": messages,
		"count":    len(messages),
	})
}

func signupHandler(c echo.Context) error {
	log.Println("signup api hit")
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

	// Insert new user
	var userID string
	err = db.QueryRow(`
		INSERT INTO users (email, user_name, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id
	`, u.Email, u.UserName, string(hashedPassword)).Scan(&userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create user"})
	}

	// Generate access token
	accessToken, err := generateJWT(userID, 30*time.Minute)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create access token"})
	}

	// Generate refresh token
	refreshToken, err := generateRefreshToken()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create refresh token"})
	}

	expiry := time.Now().Add(7 * 24 * time.Hour)

	// Store refresh token
	_, err = db.Exec(`
		INSERT INTO refresh_tokens (user_id, token, expires_at)
		VALUES ($1, $2, $3)
	`, userID, refreshToken, expiry)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to store refresh token"})
	}

	// Set httpOnly refresh token cookie
	cookie := &http.Cookie{
		Name:     "refresh_token",
		Value:    refreshToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, // true in production
		SameSite: http.SameSiteLaxMode,
		Expires:  expiry,
	}
	http.SetCookie(c.Response(), cookie)

	return c.JSON(http.StatusCreated, map[string]interface{}{
		"message":      "User registered successfully",
		"access_token": accessToken,
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
