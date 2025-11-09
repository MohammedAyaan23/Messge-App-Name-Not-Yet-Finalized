package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	Email    string `json:"email"`
	UserName string `json:"username"`
	Password string `json:"password"`
}
type SearchResult struct {
	ID       string `json:"id"`
	UserName string `json:"username"`
}

const (
	DB_USER     = "msg_admin"
	DB_PASSWORD = "54321"
	DB_NAME     = "message_app"
	DB_HOST     = "localhost"
	DB_PORT     = "5432"
)

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

var db *sql.DB
var jwtSecret = []byte("MOHAMMED_AYAAN_PASHA") // change this later for production
/*

for the cloud deployment

import (
	"context"
	"cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "google.golang.org/genproto/googleapis/cloud/secretmanager/v1"
)

var jwtSecret []byte

func init() {
	jwtSecret = []byte(loadSecret("projects/YOUR_PROJECT_ID/secrets/JWT_SECRET/versions/latest"))
}

func loadSecret(secretName string) string {
	ctx := context.Background()
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		log.Fatalf("Failed to create secret manager client: %v", err)
	}
	defer client.Close()

	req := &secretmanagerpb.AccessSecretVersionRequest{Name: secretName}
	result, err := client.AccessSecretVersion(ctx, req)
	if err != nil {
		log.Fatalf("Failed to access secret: %v", err)
	}

	return string(result.Payload.Data)
}


*/

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

	// Create Echo instance
	e := echo.New()

	// Routes
	e.POST("/signup", signupHandler)
	e.POST("/login", loginHandler)
	api := e.Group("/api")
	api.Use(echojwt.WithConfig(echojwt.Config{
		SigningKey: jwtSecret,
	}))
	api.GET("/search", searchHandler)

	fmt.Println("🚀 Server running on http://localhost:8080")
	e.Logger.Fatal(e.Start(":8080"))
}

func loginHandler(c echo.Context) error { // tested locally working fine
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

func signupHandler(c echo.Context) error { // tested locally working fine
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
