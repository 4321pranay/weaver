package com.example.shop.notify;

import jakarta.annotation.Resource;
import org.springframework.stereotype.Service;

/** C3: @Resource(name) picks the bean by name. */
@Service
public class AlertService {
    @Resource(name = "smsNotifier")
    private Notifier notifier;
    public void alert(String m) { notifier.send(m); }
}
