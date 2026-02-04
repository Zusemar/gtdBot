# ---- build stage ----
    FROM golang:1.24-alpine AS build
    WORKDIR /app
    
    # git нужен, чтобы go mod мог тянуть зависимости
    RUN apk add --no-cache git ca-certificates
    
    COPY go.mod go.sum ./
    RUN go mod download
    
    COPY . .
    
    # Собираем статический бинарь
    RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o gtdbot .
    
    # ---- runtime stage ----
    FROM alpine:3.20
    WORKDIR /app
    RUN apk add --no-cache ca-certificates tzdata
    
    COPY --from=build /app/gtdbot /app/gtdbot
    
    # БД будет монтироваться как volume
    ENV DB_PATH=/data/gtd.db
    ENV TZ=Europe/Moscow
    
    CMD ["/app/gtdbot"]