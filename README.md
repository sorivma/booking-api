# Booking API

REST API для бронирования аудиторий или переговорных во времени. Хранение in-memory, время принимается в RFC3339 и хранится в UTC.

## Запуск

```bash
go run ./cmd/server
```

Сервис слушает `:8080`. Для защищенных endpoint используйте `X-API-Key: dev-api-key`.

## Реализовано

- запрет пересекающихся approved бронирований для одной комнаты;
- прием RFC3339 и хранение времени в UTC;
- state machine для `pending`, `approved`, `rejected`, `cancelled`;
- optimistic concurrency через `version` или `If-Match`;
- middleware: structured logging, request id, API key auth, body limit, recoverer, timeout;
- `/health` и `/ready`.

## Проверка

```bash
go test ./...
```
