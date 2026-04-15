# cams

DIY security camera system. ESP32-S3 boards with OV3660 sensors stream live JPEG over TCP to a Go server that records MP4, serves a live MJPEG web UI, and lets you tweak camera settings remotely.

```
[ESP32-S3 cam0] ──TCP:7001──┐
[ESP32-S3 cam1] ──TCP:7001──┤  Go server  ──HTTP:7000──> browser
                             └─ ffmpeg records MP4 segments to disk
```

## Hardware

Tested on **GOOUUU ESP32-S3-N16R8 v1.3** (16 MB flash, 8 MB OPI PSRAM) with an OV3660 camera module. Any ESP32-S3 board with PSRAM and an OV2640/OV3660 should work — adjust the pin defines at the top of `esp32cam/main/main.c` if needed.

Camera pin mapping (current):

| Signal | GPIO |
|--------|------|
| XCLK   | 15   |
| SIOD   | 4    |
| SIOC   | 5    |
| D7–D0  | 16,17,18,12,10,8,9,11 |
| VSYNC  | 6    |
| HREF   | 7    |
| PCLK   | 13   |

## Firmware

### Prerequisites

- [ESP-IDF v5.4+](https://docs.espressif.com/projects/esp-idf/en/stable/esp32s3/get-started/)
- `idf.py` on your PATH

### Configure

Edit `esp32cam/main/main.c` — fill in your WiFi credentials, server IP, and camera ID:

```c
#define WIFI_SSID   "YOUR_SSID"
#define WIFI_PASS   "YOUR_PASS"
#define SERVER_HOST "YOUR_SERVER_IP"
#define SERVER_TCP  7001   // raw frame stream
#define SERVER_HTTP 7000   // settings polling
#define CAM_ID      "cam0" // "cam1" for the second unit
```

### Build & flash

```bash
cd esp32cam
idf.py build
idf.py flash
idf.py monitor   # optional, to see logs
```

The camera will connect to WiFi, open a persistent TCP connection to the server, and push JPEG frames as fast as the sensor produces them (~10–15 fps at VGA with OV3660). It polls `/cmd/<id>` every 5 seconds to pick up settings changes.

## Server

### Prerequisites

- Go 1.21+
- `ffmpeg` in PATH (for MP4 recording)

### Build

```bash
cd server
go build -o cams-server .
# cross-compile for Linux amd64:
GOOS=linux GOARCH=amd64 go build -o cams-server .
```

### Run

```bash
CAMS_ADDR=:7000 \
CAMS_TCP_ADDR=:7001 \
CAMS_REC_DIR=/path/to/recordings \
CAMS_USER=admin \
CAMS_PASS=yourpassword \
./cams-server
```

Environment variables (all optional, these are the defaults):

| Variable        | Default        | Description                     |
|-----------------|----------------|---------------------------------|
| `CAMS_ADDR`     | `:7000`        | HTTP listen address             |
| `CAMS_TCP_ADDR` | `:7001`        | TCP frame ingest address        |
| `CAMS_REC_DIR`  | `recordings`   | Directory for MP4 segments      |
| `CAMS_USER`     | `admin`        | Basic auth username             |
| `CAMS_PASS`     | `changeme`     | Basic auth password — change it |

### systemd

Copy and edit `server/cams.service`, then:

```bash
cp cams-server /opt/cams-server
cp cams.service /etc/systemd/system/
systemctl enable --now cams
```

Open the required ports if you use a firewall:

```bash
ufw allow 7000/tcp
ufw allow 7001/tcp
```

## API

All endpoints except `/frame/` and TCP ingest require HTTP Basic Auth.

| Endpoint          | Description                                  |
|-------------------|----------------------------------------------|
| `GET /live/<id>`  | MJPEG stream (use as `<img>` src)            |
| `GET /snap/<id>`  | Latest JPEG snapshot                         |
| `GET /cmd/<id>`   | Current camera settings (JSON)               |
| `POST /cmd/<id>`  | Update camera settings (JSON)                |
| `GET /status`     | Online/offline status for all cameras (JSON) |
| `GET /rec/<id>/`  | Browse recorded MP4 segments                 |

Settings JSON shape:

```json
{
  "brightness":   0,
  "contrast":     0,
  "saturation":  -2,
  "ae_level":     0,
  "night_mode":   false,
  "jpeg_quality": 12,
  "frame_size":   5
}
```

`frame_size` values: `5` = VGA, `8` = SVGA, `9` = XGA, `10` = HD.

Recordings are stored as 10-minute MP4 segments under `<rec_dir>/<cam_id>/<YYYY-MM-DD>/HH-MM-SS.mp4`. Oldest files are deleted automatically when total size exceeds 1 GB (configurable in `main.go`).

## Web UI

The server embeds a single-page UI at `/`. Open `http://<server>:7000` in a browser — it shows live feeds for both cameras, online/offline indicators, and collapsible per-camera settings panels (brightness, contrast, night mode, frame size, JPEG quality).
