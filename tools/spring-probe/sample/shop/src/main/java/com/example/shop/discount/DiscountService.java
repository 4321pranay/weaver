package com.example.shop.discount;

import java.util.Map;
import org.springframework.context.ApplicationContext;
import org.springframework.stereotype.Service;

@Service
public class DiscountService {
    private final Map<String, DiscountRule> rules;     // C12: every DiscountRule bean, keyed by name
    private final ApplicationContext ctx;

    public DiscountService(Map<String, DiscountRule> rules, ApplicationContext ctx) {
        this.rules = rules;
        this.ctx = ctx;
    }

    public long byMap(String code, long c) { return rules.get(code).apply(c); }

    /** C13a: bean name built at runtime. */
    public long byName(String code, long c) { return ((DiscountRule) ctx.getBean(code)).apply(c); }

    /** C13b: class name from data, plain reflection. */
    public long byReflection(String cls, long c) throws Exception {
        return ((DiscountRule) Class.forName(cls).getDeclaredConstructor().newInstance()).apply(c);
    }
}
