package com.example.shop.order;

import jakarta.persistence.Entity;
import jakarta.persistence.Id;

/** C8b: no @Table, Boot's naming strategy gives shipment_log. */
@Entity
public class ShipmentLog {
    @Id
    private Long id;
}
