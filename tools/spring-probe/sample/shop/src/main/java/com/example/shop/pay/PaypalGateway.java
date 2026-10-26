package com.example.shop.pay;

import org.springframework.stereotype.Component;

@Component
public class PaypalGateway implements PaymentGateway { public String charge(long c) { return "paypal"; } }
