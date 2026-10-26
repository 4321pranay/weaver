package com.example.shop.order;

import org.springframework.data.jpa.repository.JpaRepository;

/** C8: Spring Data proxy; the table comes from the entity. */
public interface OrderRepository extends JpaRepository<OrderEntity, Long> {}
