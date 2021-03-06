module github.com/noah-blockchain/noah-extender

go 1.12

replace mellium.im/sasl v0.2.1 => github.com/mellium/sasl v0.2.1

require (
	github.com/dgraph-io/badger v1.6.0
	github.com/go-pg/pg v8.0.5+incompatible
	github.com/golang-migrate/migrate/v4 v4.7.0
	github.com/golang/protobuf v1.3.2
	github.com/google/uuid v1.1.1
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.1 // indirect
	github.com/nats-io/nats-server/v2 v2.1.2 // indirect
	github.com/nats-io/nats-streaming-server v0.16.2 // indirect
	github.com/nats-io/stan.go v0.5.2
	github.com/noah-blockchain/coinExplorer-tools v0.1.7
	github.com/noah-blockchain/noah-explorer-tools v0.1.1
	github.com/noah-blockchain/noah-go-node v0.2.0
	github.com/noah-blockchain/noah-node-go-api v0.1.2
	github.com/pkg/errors v0.8.1
	github.com/prometheus/client_golang v1.1.0
	github.com/sirupsen/logrus v1.4.2
	mellium.im/sasl v0.2.1 // indirect
)
