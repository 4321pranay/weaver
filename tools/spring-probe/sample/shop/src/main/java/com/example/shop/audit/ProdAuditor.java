package com.example.shop.audit;

import org.springframework.context.annotation.Profile;
import org.springframework.stereotype.Component;

@Profile("prod")
@Component
public class ProdAuditor implements Auditor { public void record(String e) {} }
