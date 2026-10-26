package com.example.shop.audit;

import org.springframework.context.annotation.Profile;
import org.springframework.stereotype.Component;

@Profile("!prod")
@Component
public class DevAuditor implements Auditor { public void record(String e) {} }
