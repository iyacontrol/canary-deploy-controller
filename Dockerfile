FROM golang:1.10 as builder
WORKDIR /go/src/k8s.io/canary-deploy-controller
ADD ./  /go/src/k8s.io/canary-deploy-controller
RUN CGO_ENABLED=0 go build


FROM alpine
RUN apk add -U tzdata
RUN ln -sf /usr/share/zoneinfo/Asia/Shanghai  /etc/localtime
COPY --from=builder /go/src/k8s.io/canary-deploy-controller/canary-deploy-controller /usr/local/bin/canary-deploy-controller
RUN chmod +x /usr/local/bin/canary-deploy-controller
CMD ["canary-deploy-controller"]