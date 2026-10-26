package com.example.shop.order;

import jakarta.persistence.Entity;
import jakarta.persistence.Id;
import jakarta.persistence.Table;

@Entity
@Table(name = "t_order")
public class OrderEntity {
    @Id
    private Long id;
    private String sku;
}
