package com.example.shop.client;

import org.springframework.cloud.openfeign.FeignClient;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;

/** C7: no implementation in the code; Feign builds a proxy that makes REST calls. */
@FeignClient(name = "inventory", url = "${inventory.url}", path = "/api")
public interface InventoryClient {
    @GetMapping("/stock/{sku}")
    int stock(@PathVariable("sku") String sku);

    @PostMapping("/reserve/{sku}")
    void reserve(@PathVariable("sku") String sku);
}
