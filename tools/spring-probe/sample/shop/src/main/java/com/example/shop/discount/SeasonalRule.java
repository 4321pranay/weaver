package com.example.shop.discount;

import org.springframework.stereotype.Component;

@Component("seasonal")
public class SeasonalRule implements DiscountRule { public long apply(long c) { return c - 100; } }
