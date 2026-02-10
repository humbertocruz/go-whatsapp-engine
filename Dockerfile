# Etapa de Build
FROM golang:1.23-alpine AS builder

# Instalar dependências para o CGO (necessário para o SQLite)
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Compilar o binário habilitando CGO para o SQLite
RUN CGO_ENABLED=1 GOOS=linux go build -o engine main.go

# Etapa Final (Imagem enxuta)
FROM alpine:latest

RUN apk add --no-cache ca-certificates sqlite-libs

WORKDIR /app

# Copiar o binário da etapa anterior
COPY --from=builder /app/engine .

# Expor a porta da API
EXPOSE 3002

# Comando para rodar
CMD ["./engine"]
