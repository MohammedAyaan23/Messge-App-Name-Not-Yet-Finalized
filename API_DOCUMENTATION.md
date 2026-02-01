# API Documentation

This document outlines the API endpoints available in the application, including their HTTP methods, required inputs (headers, parameters, body), and possible responses.

## Base URL
`https://your-app.onrender.com` (Base URL)

---

## Public Routes

### 1. Signup
Registers a new user.

- **URL**: `https://your-app.onrender.com/signup`
- **Method**: `POST`
- **Auth Required**: No

#### Request
**Headers**:
- `Content-Type: application/json`

**Body**:
```json
{
  "email": "user@example.com",
  "username": "johndoe",
  "password": "securepassword"
}
```

#### Responses
**Success (201 Created)**:
- **Body**:
  ```json
  {
    "message": "User registered successfully",
    "access_token": "<jwt_access_token>"
  }
  ```
- **Cookies**: Sets `refresh_token` (HttpOnly).

**Failure**:
- **400 Bad Request**:
  - `{"error": "Invalid request body"}`
  - `{"error": "All fields are required"}`
- **409 Conflict**:
  - `{"error": "Email already registered"}`
- **500 Internal Server Error**:
  - `{"error": "Database error"}`
  - `{"error": "Password hashing failed"}`
  - `{"error": "Failed to create user"}`

---

### 2. Login
Authenticates an existing user.

- **URL**: `https://your-app.onrender.com/login`
- **Method**: `POST`
- **Auth Required**: No

#### Request
**Headers**:
- `Content-Type: application/json`

**Body**:
```json
{
  "email": "user@example.com",
  "password": "securepassword"
}
```

#### Responses
**Success (200 OK)**:
- **Body**:
  ```json
  {
    "message": "Login successful",
    "access_token": "<jwt_access_token>"
  }
  ```
- **Cookies**: Sets/Updates `refresh_token` (HttpOnly).

**Failure**:
- **400 Bad Request**:
  - `{"error": "Invalid request body"}`
  - `{"error": "Email and password are required"}`
- **401 Unauthorized**:
  - `{"error": "Invalid email or password"}`
- **500 Internal Server Error**:
  - `{"error": "Database error"}`
  - `{"error": "Failed to create access token"}`

---

### 3. Refresh Token
Refreshes an expired access token using the HttpOnly refresh token cookie.

- **URL**: `https://your-app.onrender.com/refresh`
- **Method**: `POST`
- **Auth Required**: Indirectly via Cookie

#### Request
**Cookies**:
- `refresh_token`: <valid_refresh_token>

#### Responses
**Success (200 OK)**:
- **Body**:
  ```json
  {
    "access_token": "<new_jwt_access_token>"
  }
  ```
- **Cookies**: Rotates `refresh_token` (HttpOnly).

**Failure**:
- **401 Unauthorized**:
  - `{"error": "Missing refresh token"}`
  - `{"error": "Invalid refresh token"}`
  - `{"error": "Refresh token expired"}`
- **500 Internal Server Error**:
  - `{"error": "Database error"}`

---

## Protected Routes (JWT)

All protected routes require the `Authorization` header.

**Headers**:
- `Authorization: Bearer <jwt_access_token>`

**Common Auth Failures**:
- **401 Unauthorized**:
  - `{"error": "missing_authorization_header"}`
  - `{"error": "invalid_authorization_header"}`
  - `{"error": "invalid_token"}`
  - `{"error": "access_token_expired", "detail": "Token has expired", "code": "TOKEN_EXPIRED"}`

---

### 4. User Search
Search for other users by username.

- **URL**: `https://your-app.onrender.com/api/search`
- **Method**: `GET`

#### Request
**Query Parameters**:
- `username` (Required): Partial or full username to search for.

#### Responses
**Success (200 OK)**:
- **Body**:
  ```json
  {
    "results": [
      {
        "id": "<user_uuid>",
        "username": "result_user_name"
      }
    ]
  }
  ```

**Failure**:
- **400 Bad Request**:
  - `{"error": "username query parameter required"}`
- **500 Internal Server Error**:
  - `{"error": "Database query failed"}`

---

### 5. Conversation History
Fetch message history for the authenticated user.

- **URL**: `https://your-app.onrender.com/api/conversation`
- **Method**: `GET`

#### Request
**Query Parameters**:
- `contact_id` (Optional): ID of the specific user to fetch history with. If omitted, returns all messages for the user (across all contacts).

#### Responses
**Success (200 OK)**:
- **Body**:
  ```json
  {
    "messages": [
      {
        "message_id": "...",
        "sender_id": "...",
        "receiver_id": "...",
        "content": "...",
        "delivered": true,
        "created_at": "..."
      }
    ],
    "count": 10
  }
  ```

**Failure**:
- **500 Internal Server Error**:
  - `{"error": "Database query failed"}`
  - `{"error": "Failed to retrieve messages"}`

---

## WebSocket

### 6. WebSocket Connection
Establishes a WebSocket connection for real-time messaging.

- **URL**: `wss://your-app.onrender.com/ws`
- **Method**: `GET`

#### Request
**Query Parameters**:
- `token` (Required): JWT access token.

#### Responses
**Success**:
- **101 Switching Protocols**: Connection upgraded to WebSocket.

**Special Case (Expired Token)**:
If the `token` in param is expired, but a valid `refresh_token` cookie exists:
- **200 OK** (No Upgrade):
  ```json
  {
    "error": "access_token_expired",
    "new_access_token": "<new_jwt_token>",
    "retry_ws": true
  }
  ```
  *Client should retry connection with the new access token.*

**Failure**:
- **401 Unauthorized**:
  - `{"error": "missing access token"}`
  - `{"error": "invalid access token"}`
  - `{"error": "refresh token missing"}` (if also expired)
- **500 Internal Server Error**

---

## System

### 7. Health Check
Checks if the API and Database are running.

- **URL**: `https://your-app.onrender.com/health`
- **Method**: `GET`

#### Responses
**Success (200 OK)**:
- Body: `OK`

**Failure (500 Internal Server Error)**:
- Body: `Database down`
