package com.example.shop.jobs;

import com.example.shop.handler.OrderEvent;
import com.xxl.job.core.handler.annotation.XxlJob;
import org.springframework.context.event.EventListener;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

/** C11: methods only the framework calls. */
@Component
public class Listeners {
    @Scheduled(cron = "${jobs.reconcile.cron}")
    public void reconcile() {}

    @KafkaListener(topics = "${topics.orders}", groupId = "shop")
    public void onOrder(String msg) {}

    @EventListener
    public void onOrderEvent(OrderEvent e) {}

    @XxlJob("syncInventoryJob")
    public void syncInventory() {}
}
