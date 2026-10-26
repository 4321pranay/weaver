package com.example.shop.order;

import com.example.autoconfig.FraudClient;
import com.example.shop.audit.Auditor;
import com.example.shop.cache.OrderCache;
import com.example.shop.client.InventoryClient;
import com.example.shop.handler.Handler;
import com.example.shop.handler.OrderEvent;
import com.example.shop.mapper.UserMapper;
import com.example.shop.notify.NotificationWorker;
import com.example.shop.pay.PaymentGateway;
import org.springframework.context.ApplicationEventPublisher;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

@Service
public class OrderService {
    private final PaymentGateway gateway;          // C1: two impls, @Primary wins
    private final OrderCache cache;                // C4: chosen by @ConditionalOnProperty
    private final Auditor auditor;                 // C5: chosen by @Profile
    private final FraudClient fraud;               // C6: from auto-config outside the scan
    private final InventoryClient inventory;       // C7: Feign proxy
    private final OrderRepository orders;          // C8: Spring Data proxy
    private final UserMapper users;                // C9: MyBatis-Plus proxy
    private final Handler<OrderEvent> handler;     // C16: generic type picks OrderHandler
    private final NotificationWorker worker;       // C10: @Async proxy
    private final ApplicationEventPublisher events;

    public OrderService(PaymentGateway gateway, OrderCache cache, Auditor auditor, FraudClient fraud,
                        InventoryClient inventory, OrderRepository orders, UserMapper users,
                        Handler<OrderEvent> handler, NotificationWorker worker, ApplicationEventPublisher events) {
        this.gateway = gateway; this.cache = cache; this.auditor = auditor; this.fraud = fraud;
        this.inventory = inventory; this.orders = orders; this.users = users; this.handler = handler;
        this.worker = worker; this.events = events;
    }

    @Transactional                                  // C10: transactional proxy
    public void place(long id, String sku) {
        if (!fraud.ok(id) || inventory.stock(sku) <= 0) return;
        gateway.charge(100);
        cache.put(id);
        auditor.record("placed");
        handler.handle(new OrderEvent(id));
        worker.notifyLater(id);
        events.publishEvent(new OrderEvent(id));
    }
}
