package com.example.shop.pay;

import org.springframework.beans.factory.annotation.Qualifier;
import org.springframework.stereotype.Service;

/** C2: @Qualifier on the constructor parameter beats @Primary. */
@Service
public class RefundService {
    private final PaymentGateway gateway;
    public RefundService(@Qualifier("paypalGateway") PaymentGateway gateway) { this.gateway = gateway; }
    public String refund(long c) { return gateway.charge(-c); }
}
