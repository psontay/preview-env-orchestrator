# syntax=docker/dockerfile:1

# ---- build stage ----
# Dùng đúng dòng Go trong go.mod (1.25). Alpine để layer nhỏ, có sẵn git
# cho trường hợp module cần checkout.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Tải dependency trước, tách layer để cache lại khi chỉ code đổi.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 -> binary tĩnh, chạy được trên image distroless/static.
# -trimpath + -ldflags "-s -w" để bỏ path tuyệt đối và symbol table.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# ---- run stage ----
# distroless/static: chỉ có CA certs + tzdata + /etc/passwd, không shell,
# không package manager -> bề mặt tấn công tối thiểu. Tag :nonroot chạy
# bằng user không phải root.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/api /app/api
# Kèm migrations để tiện chạy thủ công / soi trong container khi debug.
COPY --from=build /src/migrations /app/migrations

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/app/api"]
