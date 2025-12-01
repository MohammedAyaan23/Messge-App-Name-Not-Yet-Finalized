# Message App (Name TBD)

A backend implementation of a real-time messaging application built with Go, featuring WebSocket connections, JWT authentication, and PostgreSQL database integration.

![Image](https://github.com/MohammedAyaan23/Messge-App-Name-Not-Yet-Finalized/blob/ddf10eb3d3409b7c31b4b90fc080eca319a7964a/yo.mp4)

## Project Motivation

This project was created as a learning journey into Go development, specifically to understand:
- Go syntax and language fundamentals
- Concurrency patterns (goroutines and channels)
- Real-time communication with WebSockets
- REST API design and implementation

While messaging apps are a common project, this serves as a practical application for mastering Go's core concepts and building production-ready backend services [web:1][web:2].

## Features

- User authentication (signup/login) with JWT tokens
- Real-time messaging via WebSocket connections
- User search functionality with similarity matching
- Conversation history retrieval
- Secure password hashing
- PostgreSQL database for persistent storage

## Tech Stack

- **Language**: Go
- **Web Framework**: Echo (REST endpoints)
- **WebSocket**: Gorilla WebSocket
- **Database**: PostgreSQL
- **Authentication**: JWT (access & refresh tokens)

## Getting Started

### Prerequisites

- Go installed ([official website](https://golang.org/))
- PostgreSQL (`brew install postgresql`)
- Familiarity with Go basics (loops, conditionals, error handling, goroutines)

### Installation

1. Clone the repository
2. Install dependencies listed in `go.mod`
3. Set up PostgreSQL database with three tables:
   - `users` - stores email, username, hashed passwords
   - `refresh_tokens` - stores tokens with expiration dates
   - `messages` - stores message data
4. Configure appropriate indexing for optimized queries

### Running the Application

[Add specific commands here for running your server]

## Architecture Overview

### Authentication Flow

**Signup/Login**
- Client sends POST request with credentials
- Server validates and hashes password (using bcrypt or similar)
- Returns short-lived access token (JWT)
- Stores long-lived refresh token securely [web:3]

**Protected Endpoints**
All subsequent API calls require the access token in headers. The JWT middleware:
1. Parses and validates the token
2. Checks expiration status
3. Automatically refreshes using refresh token if expired
4. Returns 401 Unauthorized if invalid

### REST API Endpoints

- `POST /login` - User authentication
- `POST /signup` - User registration
- `GET /search` - Find users by username (requires JWT)
- `GET /conversation` - Fetch message history (requires JWT)

### WebSocket Connection

Unlike traditional REST APIs (unidirectional, short-lived request-response cycles), WebSockets establish persistent, bidirectional connections enabling real-time communication [web:7].

**Connection Flow:**
1. Client initiates handshake with access token
2. Server validates credentials via WebSocket handler
3. HTTP connection upgrades to WebSocket protocol
4. Bidirectional message streaming begins
5. All messages persist to PostgreSQL database

**Analogy**: REST is like exchanging letters (send, wait, receive), while WebSocket is like a phone call (continuous two-way conversation).

## Security Practices

- Passwords are hashed (not encrypted) using one-way cryptographic functions
- Access tokens are short-lived to minimize compromise risk
- Refresh tokens stored securely (cookies for web, keychain for iOS)
- Never expose refresh tokens in client-side code [web:5]

## Learning Resources

- [Official Go Tutorial](https://go.dev/tour/)
- [Understanding Goroutines](https://www.youtube.com/watch?v=qyM8Pi1KiiM)
- Trevor Noah standup (for cultural context on "the beginning")

## Future Enhancements

- API Gateway implementation
- Load balancer integration
- Enhanced production-grade architecture
- [Additional features you're planning]

