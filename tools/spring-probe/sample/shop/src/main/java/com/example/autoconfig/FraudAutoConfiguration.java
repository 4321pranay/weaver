package com.example.autoconfig;

import org.springframework.boot.autoconfigure.AutoConfiguration;
import org.springframework.boot.autoconfigure.condition.ConditionalOnClass;
import org.springframework.boot.autoconfigure.condition.ConditionalOnMissingBean;
import org.springframework.context.annotation.Bean;

/** C6: auto-configuration outside the component-scan package, registered via AutoConfiguration.imports. */
@AutoConfiguration
public class FraudAutoConfiguration {
    @Bean
    @ConditionalOnMissingBean
    public FraudClient defaultFraudClient() { return new HttpFraudClient(); }

    /** C15: needs a class that isn't on the classpath, so the bean doesn't exist. */
    @Bean
    @ConditionalOnClass(name = "com.missing.FancyLib")
    public FraudClient fancyFraudClient() { return new HttpFraudClient(); }
}
