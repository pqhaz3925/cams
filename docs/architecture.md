# Architecture

## Current (direct to server)

```mermaid
flowchart LR
    cam0[ESP32-S3 cam0<br/>OV3660]
    cam1[ESP32-S3 cam1<br/>OV3660]

    subgraph netcup[netcup VPS]
        srv[Go server<br/>:7000 HTTP<br/>:7001 TCP]
        ff[ffmpeg<br/>MJPEG → MP4<br/>10-min segments]
        disk[(recordings/<br/>1 GB cap)]
        srv --> ff --> disk
    end

    browser[Browser<br/>MJPEG live UI]

    cam0 -- JPEG over TCP --> srv
    cam1 -- JPEG over TCP --> srv
    srv -- MJPEG + UI --> browser
    srv -. settings poll .-> cam0
    srv -. settings poll .-> cam1
```

## Target (Pi aggregator on LAN)

```mermaid
flowchart LR
    cam0[ESP32-S3 cam0]
    cam1[ESP32-S3 cam1]
    cam2[ESP32-S3 cam2]
    mic[I2S mic<br/>INMP441?]

    subgraph home[Home LAN]
        router[Router]
        subgraph pi[Orange Pi Zero 3 2GB]
            proxy[Go proxy<br/>TCP ingest :7001]
            enc[ffmpeg<br/>MJPEG → H.264<br/>30× bandwidth cut]
            yolo[YOLOv5n<br/>motion detect<br/>~1-2 fps]
            ring[(Ring buffer<br/>USB HDD / SD)]
            proxy --> enc
            proxy --> yolo
            enc --> ring
        end
        cam0 -- WiFi --> router
        cam1 -- WiFi --> router
        cam2 -- WiFi --> router
        mic -. via cam0? .-> router
        router --> pi
    end

    subgraph netcup[netcup VPS]
        srv[Go server<br/>live relay + archive]
    end

    browser[Browser]

    pi -- H.264 fMP4 over LAN/WAN --> srv
    pi -. alerts on motion .-> srv
    srv --> browser
```

## Data flow per frame (target)

```
OV3660 sensor
    ↓ parallel DVP bus
ESP32-S3 JPEG encoder (hardware)
    ↓ persistent TCP
Orange Pi Go proxy
    ↓ pipe
ffmpeg (HW H.264 via Cedrus/V4L2)
    ├→ local ring buffer (USB disk)
    ├→ YOLO inference (every Nth frame)
    └→ upload stream to netcup
```

## Why a Pi in the middle

| Concern              | Direct to VPS       | Pi aggregator             |
|----------------------|---------------------|---------------------------|
| Bandwidth to VPS     | ~24 Mbps / cam MJPEG | ~1 Mbps / cam H.264       |
| Survives WAN outage  | no (drops frames)   | yes (local ring buffer)   |
| Motion detection     | needs VPS CPU       | runs locally, free        |
| Single point of upload | per-cam WiFi→WAN  | one wired LAN→WAN link   |
| Added complexity     | none                | one more box to maintain  |
