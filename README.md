# go-draw

A lightweight real-time multiplayer drawing and guessing game written in Go.

The server serves a single-page web app and uses WebSockets to synchronize drawing operations, game turns, chat, and scoring between connected players.

## Features

- Real-time collaborative canvas updates over WebSockets
- Turn-based draw-and-guess gameplay
- Scoreboard with per-player colors and scores
- Join as player or watch as spectator
- Built-in chat and guess stream
- Embedded static frontend assets (no external frontend build step)

## Tech Stack

- Go (`net/http`, `embed`)
- [gorilla/websocket](https://github.com/gorilla/websocket)
- Plain HTML/CSS/JavaScript frontend (served from `static/`)

## Requirements

- Go 1.25.9 (as declared in `go.mod`)

## Run Locally

```bash
go run .
```

Then open:

- `http://localhost:8080`

## Build

```bash
go build ./...
```

## Test

```bash
go test ./...
```

## Docker

Build image:

```bash
docker build -t go-draw .
```

Run container:

```bash
docker run --rm -p 8080:8080 go-draw
```

Open `http://localhost:8080`.

## Deployment

This repository includes `fly.toml` and a multi-stage `Dockerfile` suitable for Fly.io deployment.

## Project Structure

- `main.go` — WebSocket hub, game state, and HTTP server
- `static/index.html` — client UI and gameplay logic
- `Dockerfile` — container build/runtime image
- `fly.toml` — Fly.io app configuration

## License

See [LICENSE](LICENSE).
