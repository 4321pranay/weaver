# Service Map Examples

One `edges.yaml` entry per edge type, for [edges-spec.md](edges-spec.md).

Each example below is one `edges.yaml` entry for one edge type. Symbols are
shortened to `…` after the package part.
Nodes used:

```yaml
nodes:
  - {id: checkout, type: service, props: {domain: payments}}
  - {id: payment,  type: service, props: {domain: payments}}
  - {id: shipping, type: service, props: {domain: logistics}}
  - {id: notifier, type: service, props: {domain: payments}}
  - {id: database/orders,   type: database, props: {engine: postgres, database: orders, host: orders-db.prod:5432}}
  - {id: ceph/invoices,     type: ceph,     props: {endpoint: s3.ceph.prod, bucket: invoices}}
  - {id: redis/cache.prod,  type: redis,    props: {host: cache.prod:6379, db: 0}}
  - {id: kafka/orders, type: kafka, aliases: [kafka-1.prod:9092, kafka-2.prod:9092], props: {brokers: [kafka-1.prod:9092, kafka-2.prod:9092]}}
```

## 1. Service → service (REST)

`checkout` calls payment's `POST /pay`. Placing an order fails if the payment
fails, while a refund only logs the error.

```yaml
- from: checkout
  to: payment
  kind: rest
  resource: POST /pay
  call: required
  handler: {symbol: "maven/com.shop/payment . shop/PayController#pay().", at: src/main/java/shop/PayController.java:31}
  callers:
    - at: src/main/java/shop/CheckoutService.java:42
      call: required
      method:   {symbol: "… shop/CheckoutService#placeOrder().", at: src/main/java/shop/CheckoutService.java:30}
      called_by: [{symbol: "… shop/OrderController#create().", at: src/main/java/shop/OrderController.java:22}]
      entries:   [{kind: rest, resource: POST /orders, symbol: "… shop/OrderController#create().", at: src/main/java/shop/OrderController.java:22}]
    - at: src/main/java/shop/RefundService.java:88
      call: optional
      method:   {symbol: "… shop/RefundService#refund().", at: src/main/java/shop/RefundService.java:80}
      called_by: [{symbol: "… shop/RefundJob#run().", at: src/main/java/shop/RefundJob.java:15}]
      entries:   [{kind: schedule, resource: "0 */5 * * * *", symbol: "… shop/RefundJob#run().", at: src/main/java/shop/RefundJob.java:15}]
```

`handler` is payment's handler for the endpoint. Each entry in `callers` is a place
in checkout that calls it.

## 2. Service → database

`checkout` writes the `orders` table and reads `customers`. There is one edge
per table.

```yaml
- from: checkout
  to: database/orders
  kind: db
  resource: orders
  op: write
  call: required
  callers:
    - at: src/main/java/shop/OrderRepository.java:12
      call: required
      method:   {symbol: "… shop/OrderRepository#save().", at: src/main/java/shop/OrderRepository.java:10}
      called_by: [{symbol: "… shop/CheckoutService#placeOrder().", at: src/main/java/shop/CheckoutService.java:38}]

- from: checkout
  to: database/orders
  kind: db
  resource: customers
  op: read
  call: required
  callers:
    - at: src/main/java/shop/CustomerRepository.java:9
      call: required
      method:   {symbol: "… shop/CustomerRepository#findById().", at: src/main/java/shop/CustomerRepository.java:9}
      called_by: [{symbol: "… shop/CheckoutService#placeOrder().", at: src/main/java/shop/CheckoutService.java:33}]
```

There is no `handler`, because the far end is infrastructure, not code.

## 3. Service → Ceph (S3 API)

`checkout` uploads invoice PDFs. A failed upload is retried later, so it
doesn't fail the order.

```yaml
- from: checkout
  to: ceph/invoices
  kind: s3
  resource: "invoices/{}"
  op: write
  call: optional
  callers:
    - at: src/main/java/shop/InvoiceStore.java:27
      call: optional
      method:   {symbol: "… shop/InvoiceStore#upload().", at: src/main/java/shop/InvoiceStore.java:20}
      called_by: [{symbol: "… shop/CheckoutService#placeOrder().", at: src/main/java/shop/CheckoutService.java:45}]
```

`kind: s3` is the protocol, while the node's `type` is `ceph`. The key
`invoices/2026/10/<orderId>.pdf` is normalised to the prefix `invoices/{}`.

## 4. Service → Redis (keys)

`checkout` caches carts. On a Redis error it loads the cart from the database
instead.

```yaml
- from: checkout
  to: redis/cache.prod
  kind: redis
  resource: "cart:{}"
  op: readwrite
  call: fallback
  callers:
    - at: src/main/java/shop/CartCache.java:30
      call: fallback
      method:   {symbol: "… shop/CartCache#get().", at: src/main/java/shop/CartCache.java:26}
      called_by: [{symbol: "… shop/CheckoutService#placeOrder().", at: src/main/java/shop/CheckoutService.java:31}]
```

## 5. Service → Kafka (produce)

`checkout` publishes `orders.created` and doesn't wait for the result.

```yaml
- from: checkout
  to: kafka/orders
  kind: kafka
  resource: orders.created
  op: produce
  call: async
  callers:
    - at: src/main/java/shop/OrderPublisher.java:19
      call: async
      method:   {symbol: "… shop/OrderPublisher#publish().", at: src/main/java/shop/OrderPublisher.java:15}
      called_by: [{symbol: "… shop/CheckoutService#placeOrder().", at: src/main/java/shop/CheckoutService.java:47}]
```

## 6. Kafka → service (consume)

`shipping` consumes `orders.created`.

```yaml
- from: kafka/orders
  to: shipping
  kind: kafka
  resource: orders.created
  op: consume
  handler: {symbol: "scip-go gomod github.com/shop/shipping . internal/consumer/OrderConsumer#Handle().", at: internal/consumer/orders.go:27}
```

On a consume edge, the code is in `to`:

- `handler` is the consumer method (`@KafkaListener` method, or the Go handler).
- There are no `callers`, because the infrastructure end has no code.

The derived hop (edges-spec §3.3) is `checkout ⇒ shipping via kafka/orders#orders.created`, and it is `async`.

## 7. Redis pub/sub

`checkout` publishes to the Redis channel `order.events`, and `notifier`
subscribes to it. Pub/sub has the same shape as Kafka:

- The publish edge goes service → redis, with `op: publish`.
- The subscribe edge goes redis → service, with `op: subscribe`.

The `op` is what tells pub/sub apart from key access (`read`, `write` and
`readwrite`, §4) on the same Redis node.

```yaml
- from: checkout
  to: redis/cache.prod
  kind: redis
  resource: order.events
  op: publish
  call: async
  callers:
    - at: src/main/java/shop/EventBus.java:14
      call: async
      method:   {symbol: "… shop/EventBus#emit().", at: src/main/java/shop/EventBus.java:12}
      called_by: [{symbol: "… shop/CheckoutService#placeOrder().", at: src/main/java/shop/CheckoutService.java:49}]

- from: redis/cache.prod
  to: notifier
  kind: redis
  resource: order.events
  op: subscribe
  handler: {symbol: "… notify/OrderEventsListener#onMessage().", at: src/main/java/notify/OrderEventsListener.java:18}
```

The derived hop is `checkout ⇒ notifier via redis/cache.prod#order.events`, and it is `async`.

## 8. Blast radius over these edges

- **`blast payment`:** reaches `checkout`, because 9.1 has a `required` call.
- **`blast database/orders`:** reaches `checkout`, because 9.2 is `required`.
- **`blast ceph/invoices`, `blast redis/cache.prod` or `blast kafka/orders`:**
  none of them reach `checkout`. The calls are `optional`, `fallback` and `async`.
- **`blast kafka/orders`:** reaches `shipping`, because a consumer cannot
  work without its source.
