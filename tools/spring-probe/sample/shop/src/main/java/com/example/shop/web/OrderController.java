package com.example.shop.web;

import com.example.shop.order.OrderService;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
@RequestMapping("${api.prefix:/v1}/orders")       // C11a: prefix from a property with a default
public class OrderController {
    private final OrderService service;
    public OrderController(OrderService service) { this.service = service; }

    @PostMapping("/{sku}")
    public void place(@PathVariable String sku) { service.place(1, sku); }
}
