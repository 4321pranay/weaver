package com.example.shop.discount;

import org.springframework.stereotype.Component;

@Component("vip")
public class VipRule implements DiscountRule { public long apply(long c) { return c * 9 / 10; } }
