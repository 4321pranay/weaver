package com.example.shop.cache;

import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.stereotype.Component;

@Component
@ConditionalOnProperty(name = "cache.type", havingValue = "local", matchIfMissing = true)
public class LocalCache implements OrderCache { public void put(long id) {} }
