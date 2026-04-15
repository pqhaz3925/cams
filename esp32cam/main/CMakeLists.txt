idf_component_register(
    SRCS "main.c"
    INCLUDE_DIRS "."
    REQUIRES
        esp_http_client
        esp_wifi
        nvs_flash
        esp_netif
        driver
        esp_timer
        json
)
