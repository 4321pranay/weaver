package com.example.shop.notify;

import org.springframework.scheduling.annotation.Async;
import org.springframework.stereotype.Component;

@Component
public class NotificationWorker {
    @Async                                          // C10: runs on another thread via a proxy
    public void notifyLater(long id) {}
}
