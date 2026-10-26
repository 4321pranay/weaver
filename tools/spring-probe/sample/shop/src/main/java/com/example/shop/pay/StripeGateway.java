package com.example.shop.pay;

import org.springframework.context.annotation.Primary;
import org.springframework.stereotype.Component;

@Primary
@Component
public class StripeGateway implements PaymentGateway { public String charge(long c) { return "stripe"; } }
