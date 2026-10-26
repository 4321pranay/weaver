package com.example.shop.cache;

import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.stereotype.Component;

@Component
@ConditionalOnProperty(name = "cache.type", havingValue = "redis")
public class RedisCache implements OrderCache { public void put(long id) {} }
