#include <string.h>
#include <stdio.h>
#include <sys/socket.h>
#include <netdb.h>
#include <arpa/inet.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/event_groups.h"
#include "esp_system.h"
#include "esp_wifi.h"
#include "esp_event.h"
#include "esp_log.h"
#include "nvs_flash.h"
#include "esp_netif.h"
#include "esp_http_client.h"
#include "esp_camera.h"
#include "cJSON.h"

static const char *TAG = "esp32cam";

/* ── Config ────────────────────────────────────────────────────────────────── */
#define WIFI_SSID      "YOUR_SSID"
#define WIFI_PASS      "YOUR_PASS"
#define SERVER_HOST    "YOUR_SERVER_IP"
#define SERVER_HTTP    7000   /* cmd polling */
#define SERVER_TCP     7001   /* raw frame stream */
#define CAM_ID         "cam0" /* change to "cam1" on second unit */

#define CMD_POLL_MS    5000
#define PUSH_RETRY_MS  2000
#define WIFI_RETRY_MAX 10

/* ── Camera pins: GOOUUU ESP32-S3-N16R8 v1.3 + OV3660 ─────────────────────── */
#define CAM_PIN_PWDN   -1
#define CAM_PIN_RESET  -1
#define CAM_PIN_XCLK   15
#define CAM_PIN_SIOD    4
#define CAM_PIN_SIOC    5
#define CAM_PIN_D7     16
#define CAM_PIN_D6     17
#define CAM_PIN_D5     18
#define CAM_PIN_D4     12
#define CAM_PIN_D3     10
#define CAM_PIN_D2      8
#define CAM_PIN_D1      9
#define CAM_PIN_D0     11
#define CAM_PIN_VSYNC   6
#define CAM_PIN_HREF    7
#define CAM_PIN_PCLK   13

/* ── WiFi ──────────────────────────────────────────────────────────────────── */
#define WIFI_CONNECTED_BIT BIT0
static EventGroupHandle_t wifi_eg;
static int wifi_retry = 0;

/* ── Camera settings ───────────────────────────────────────────────────────── */
typedef struct {
    int  brightness;
    int  contrast;
    int  saturation;
    int  ae_level;
    bool night_mode;
    int  jpeg_quality;
    int  frame_size;
} cam_settings_t;

static cam_settings_t g_settings = {
    .brightness   = 0,
    .contrast     = 0,
    .saturation   = -2,
    .ae_level     = 0,
    .night_mode   = false,
    .jpeg_quality = 12,
    .frame_size   = FRAMESIZE_VGA,
};

static void apply_settings(const cam_settings_t *s)
{
    sensor_t *sensor = esp_camera_sensor_get();
    if (!sensor) return;
    sensor->set_brightness(sensor, s->brightness);
    sensor->set_contrast(sensor, s->contrast);
    sensor->set_saturation(sensor, s->saturation);
    sensor->set_ae_level(sensor, s->ae_level);
    if (s->night_mode) {
        sensor->set_gainceiling(sensor, GAINCEILING_128X);
        sensor->set_exposure_ctrl(sensor, 1);
        sensor->set_gain_ctrl(sensor, 1);
        sensor->set_agc_gain(sensor, 30);
        sensor->set_aec_value(sensor, 800);
        sensor->set_aec2(sensor, 1);
    } else {
        sensor->set_gainceiling(sensor, GAINCEILING_2X);
        sensor->set_exposure_ctrl(sensor, 1);
        sensor->set_gain_ctrl(sensor, 1);
        sensor->set_aec2(sensor, 0);
    }
    sensor->set_quality(sensor, s->jpeg_quality);
    sensor->set_framesize(sensor, (framesize_t)s->frame_size);
    ESP_LOGI(TAG, "settings: bright=%d night=%d quality=%d size=%d",
             s->brightness, s->night_mode, s->jpeg_quality, s->frame_size);
}

/* ── TCP write helper (handles partial writes) ─────────────────────────────── */
static int tcp_write_all(int sock, const void *buf, size_t len)
{
    const uint8_t *p = (const uint8_t *)buf;
    while (len > 0) {
        int n = send(sock, p, len, 0);
        if (n <= 0) return -1;
        p += n;
        len -= n;
    }
    return 0;
}

/* ── Push task — raw TCP, fire and forget ──────────────────────────────────── */
static void push_task(void *arg)
{
    struct sockaddr_in addr = {
        .sin_family = AF_INET,
        .sin_port   = htons(SERVER_TCP),
    };
    inet_aton(SERVER_HOST, &addr.sin_addr);

    uint32_t backoff_ms = PUSH_RETRY_MS;

    while (true) {
        xEventGroupWaitBits(wifi_eg, WIFI_CONNECTED_BIT, false, true, portMAX_DELAY);

        int sock = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
        if (sock < 0) {
            vTaskDelay(pdMS_TO_TICKS(backoff_ms));
            continue;
        }

        /* send timeout so we don't block forever on a dead connection */
        struct timeval tv = { .tv_sec = 5, .tv_usec = 0 };
        setsockopt(sock, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));

        if (connect(sock, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
            ESP_LOGW(TAG, "TCP connect failed — retry in %"PRIu32"ms", backoff_ms);
            close(sock);
            vTaskDelay(pdMS_TO_TICKS(backoff_ms));
            backoff_ms = (backoff_ms * 2 > 30000) ? 30000 : backoff_ms * 2;
            continue;
        }

        /* identify camera */
        const char *id_line = CAM_ID "\n";
        if (tcp_write_all(sock, id_line, strlen(id_line)) < 0) {
            close(sock);
            continue;
        }

        ESP_LOGI(TAG, "TCP streaming to %s:%d", SERVER_HOST, SERVER_TCP);
        backoff_ms = PUSH_RETRY_MS;

        while (true) {
            camera_fb_t *fb = esp_camera_fb_get();
            if (!fb) { vTaskDelay(pdMS_TO_TICKS(10)); continue; }

            uint32_t len_be = htonl((uint32_t)fb->len);
            bool ok = (tcp_write_all(sock, &len_be, 4) == 0) &&
                      (tcp_write_all(sock, fb->buf, fb->len) == 0);
            esp_camera_fb_return(fb);

            if (!ok) {
                ESP_LOGW(TAG, "TCP write failed");
                break;
            }
        }

        close(sock);
        ESP_LOGW(TAG, "TCP disconnected — retry in %"PRIu32"ms", backoff_ms);
        vTaskDelay(pdMS_TO_TICKS(backoff_ms));
        backoff_ms = (backoff_ms * 2 > 30000) ? 30000 : backoff_ms * 2;
    }
}

/* ── Cmd task — HTTP polling for settings ──────────────────────────────────── */
typedef struct { char *buf; int len; int cap; } resp_buf_t;

static esp_err_t http_evt(esp_http_client_event_t *evt)
{
    resp_buf_t *rb = (resp_buf_t *)evt->user_data;
    if (!rb) return ESP_OK;
    if (evt->event_id == HTTP_EVENT_ON_DATA) {
        int needed = rb->len + evt->data_len + 1;
        if (needed > rb->cap) {
            rb->cap = needed + 256;
            rb->buf = realloc(rb->buf, rb->cap);
        }
        if (rb->buf) {
            memcpy(rb->buf + rb->len, evt->data, evt->data_len);
            rb->len += evt->data_len;
            rb->buf[rb->len] = 0;
        }
    } else if (evt->event_id == HTTP_EVENT_DISCONNECTED) {
        rb->len = 0;
    }
    return ESP_OK;
}

static void cmd_task(void *arg)
{
    char url[128];
    snprintf(url, sizeof(url), "http://%s:%d/cmd/" CAM_ID, SERVER_HOST, SERVER_HTTP);

    resp_buf_t rb = {0};
    esp_http_client_config_t cfg = {
        .url           = url,
        .method        = HTTP_METHOD_GET,
        .timeout_ms    = 5000,
        .event_handler = http_evt,
        .user_data     = &rb,
    };
    esp_http_client_handle_t client = esp_http_client_init(&cfg);

    while (true) {
        vTaskDelay(pdMS_TO_TICKS(CMD_POLL_MS));
        xEventGroupWaitBits(wifi_eg, WIFI_CONNECTED_BIT, false, true, portMAX_DELAY);

        rb.len = 0;
        if (esp_http_client_perform(client) != ESP_OK) continue;
        if (esp_http_client_get_status_code(client) != 200) continue;
        if (!rb.buf || rb.len == 0) continue;

        cJSON *j = cJSON_ParseWithLength(rb.buf, rb.len);
        if (!j) continue;

        cam_settings_t s = g_settings;
        cJSON *v;
        if ((v = cJSON_GetObjectItem(j, "brightness"))   && cJSON_IsNumber(v)) s.brightness   = v->valueint;
        if ((v = cJSON_GetObjectItem(j, "contrast"))     && cJSON_IsNumber(v)) s.contrast      = v->valueint;
        if ((v = cJSON_GetObjectItem(j, "saturation"))   && cJSON_IsNumber(v)) s.saturation    = v->valueint;
        if ((v = cJSON_GetObjectItem(j, "ae_level"))     && cJSON_IsNumber(v)) s.ae_level      = v->valueint;
        if ((v = cJSON_GetObjectItem(j, "night_mode"))   && cJSON_IsBool(v))   s.night_mode    = cJSON_IsTrue(v);
        if ((v = cJSON_GetObjectItem(j, "jpeg_quality")) && cJSON_IsNumber(v)) s.jpeg_quality  = v->valueint;
        if ((v = cJSON_GetObjectItem(j, "frame_size"))   && cJSON_IsNumber(v)) s.frame_size    = v->valueint;
        cJSON_Delete(j);

        if (memcmp(&s, &g_settings, sizeof(s)) != 0) {
            g_settings = s;
            apply_settings(&g_settings);
        }
    }
}

/* ── Camera init ───────────────────────────────────────────────────────────── */
static esp_err_t camera_init(void)
{
    camera_config_t cfg = {
        .pin_pwdn     = CAM_PIN_PWDN,  .pin_reset    = CAM_PIN_RESET,
        .pin_xclk     = CAM_PIN_XCLK,
        .pin_sccb_sda = CAM_PIN_SIOD,  .pin_sccb_scl = CAM_PIN_SIOC,
        .pin_d7 = CAM_PIN_D7, .pin_d6 = CAM_PIN_D6,
        .pin_d5 = CAM_PIN_D5, .pin_d4 = CAM_PIN_D4,
        .pin_d3 = CAM_PIN_D3, .pin_d2 = CAM_PIN_D2,
        .pin_d1 = CAM_PIN_D1, .pin_d0 = CAM_PIN_D0,
        .pin_vsync    = CAM_PIN_VSYNC,
        .pin_href     = CAM_PIN_HREF,
        .pin_pclk     = CAM_PIN_PCLK,
        .xclk_freq_hz = 20000000,
        .ledc_timer   = LEDC_TIMER_0,
        .ledc_channel = LEDC_CHANNEL_0,
        .pixel_format = PIXFORMAT_JPEG,
        .frame_size   = FRAMESIZE_VGA,
        .jpeg_quality = 12,
        .fb_count     = 3,                      /* extra buffer so camera never waits */
        .fb_location  = CAMERA_FB_IN_PSRAM,
        .grab_mode    = CAMERA_GRAB_LATEST,     /* always return freshest frame */
    };

    esp_err_t ret = esp_camera_init(&cfg);
    if (ret != ESP_OK) { ESP_LOGE(TAG, "cam init failed: 0x%x", ret); return ret; }

    sensor_t *s = esp_camera_sensor_get();
    if (s->id.PID == OV3660_PID) s->set_vflip(s, 1);
    apply_settings(&g_settings);
    ESP_LOGI(TAG, "camera ready (PID: 0x%x)", s->id.PID);
    return ESP_OK;
}

/* ── WiFi ──────────────────────────────────────────────────────────────────── */
static void wifi_event_handler(void *arg, esp_event_base_t base,
                               int32_t id, void *data)
{
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        xEventGroupClearBits(wifi_eg, WIFI_CONNECTED_BIT);
        if (wifi_retry < WIFI_RETRY_MAX) {
            wifi_retry++;
            uint32_t delay = (1u << wifi_retry) * 500;
            if (delay > 30000) delay = 30000;
            ESP_LOGW(TAG, "WiFi lost — retry %d in %"PRIu32"ms", wifi_retry, delay);
            vTaskDelay(pdMS_TO_TICKS(delay));
            esp_wifi_connect();
        } else {
            ESP_LOGE(TAG, "WiFi retry limit — rebooting");
            esp_restart();
        }
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *e = (ip_event_got_ip_t *)data;
        ESP_LOGI(TAG, "IP: " IPSTR, IP2STR(&e->ip_info.ip));
        wifi_retry = 0;
        xEventGroupSetBits(wifi_eg, WIFI_CONNECTED_BIT);
    }
}

static void wifi_init(void)
{
    wifi_eg = xEventGroupCreate();
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();

    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&cfg));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(
        WIFI_EVENT, ESP_EVENT_ANY_ID, &wifi_event_handler, NULL, NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(
        IP_EVENT, IP_EVENT_STA_GOT_IP, &wifi_event_handler, NULL, NULL));

    wifi_config_t wcfg = {
        .sta = { .ssid = WIFI_SSID, .password = WIFI_PASS,
                 .threshold.authmode = WIFI_AUTH_WPA2_PSK },
    };
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wcfg));
    ESP_ERROR_CHECK(esp_wifi_start());
}

/* ── Entry ─────────────────────────────────────────────────────────────────── */
void app_main(void)
{
    ESP_ERROR_CHECK(nvs_flash_init());
    ESP_ERROR_CHECK(camera_init());
    wifi_init();
    xEventGroupWaitBits(wifi_eg, WIFI_CONNECTED_BIT, false, true, portMAX_DELAY);
    xTaskCreate(push_task, "push", 8192, NULL, 5, NULL);
    xTaskCreate(cmd_task,  "cmd",  6144, NULL, 4, NULL);
}
