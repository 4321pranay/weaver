package com.example.shop.jobs;

import org.apache.rocketmq.spring.annotation.RocketMQMessageListener;
import org.apache.rocketmq.spring.core.RocketMQListener;
import org.springframework.stereotype.Component;

/** C11e: RocketMQ listener declared on the class; the framework calls onMessage. */
@Component
@RocketMQMessageListener(topic = "pay-result", consumerGroup = "shop-pay")
public class PaymentConsumer implements RocketMQListener<String> {
    public void onMessage(String msg) {}
}
