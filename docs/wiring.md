# Wiring — per camera unit

## Block diagram

```mermaid
flowchart LR
    mains[AC 220V<br/>wall socket]
    psu[12V 2A<br/>wall adapter]
    buck[Buck converter<br/>LM2596 / MP1584<br/>12V → 5V, ≥1A]
    esp[ESP32-S3-N16R8<br/>onboard LDO<br/>5V → 3.3V]
    cam[OV3660<br/>on FFC ribbon]
    mic[INMP441<br/>I2S mic]

    mains -- 220V AC --> psu
    psu == 12V DC ==> buck
    buck == 5V ==> esp
    esp -. 3.3V .-> mic
    esp -- camera socket --> cam
    esp -- I2S bus --> mic
```

## Power path

```
AC 220V ──[wall adapter]──► 12V DC ──[buck converter]──► 5V ──► ESP32 VIN pin
                                                                    │
                                                            [onboard AMS1117]
                                                                    │
                                                                 3.3V rail
                                                                    │
                                                            ┌───────┴───────┐
                                                            ▼               ▼
                                                     camera (via FFC)    INMP441
```

**Why 12V → buck → 5V, not direct 5V adapter:**
- 12V runs over longer/thinner cable with less voltage drop (useful if camera is placed away from outlet)
- Lets you add IR illuminator / heater on the same 12V rail later
- Buck converters (~2₽ each) are more efficient than linear regulators when dropping multiple volts

## Pin mapping

### Camera (fixed — uses the FFC ribbon connector on the board)

| Signal | GPIO | Signal | GPIO |
|--------|------|--------|------|
| XCLK   | 15   | VSYNC  | 6    |
| SIOD   | 4    | HREF   | 7    |
| SIOC   | 5    | PCLK   | 13   |
| D0–D7  | 11, 9, 8, 10, 12, 18, 17, 16 | | |

### Microphone (INMP441 I2S) — proposed free pins

| INMP441 pin | Function     | ESP32-S3 GPIO |
|-------------|--------------|---------------|
| VDD         | 3.3V         | 3V3           |
| GND         | ground       | GND           |
| L/R         | channel sel  | GND (left)    |
| SCK         | I2S bit clk  | **GPIO 42**   |
| WS          | I2S word sel | **GPIO 41**   |
| SD          | I2S data out | **GPIO 40**   |

These GPIOs are unused by the camera driver and safe for I2S (not strapping pins).

## Physical layout inside enclosure

```
┌────────────────────── ENCLOSURE ──────────────────────┐
│                                                       │
│   [12V barrel jack]                                   │
│        │                                              │
│        ▼                                              │
│   ┌─────────┐       ┌──────────────────┐              │
│   │  buck   │──5V──►│  ESP32-S3 board  │◄── FFC ──► [OV3660 cam]
│   │ LM2596  │       │                  │              │
│   └─────────┘       │          [I2S] ──┼──► [INMP441]
│        │            └──────────────────┘              │
│       GND ═══════════ common ground                   │
│                                                       │
│                           [WiFi antenna pigtail] ─────┼──► [ext antenna]
└───────────────────────────────────────────────────────┘
```

## BOM per camera (approx)

| Part                               | Qty | Note                                |
|------------------------------------|-----|-------------------------------------|
| GOOUUU ESP32-S3-N16R8 + OV3660     | 1   | bundle, comes with FFC ribbon       |
| 12V 2A wall adapter + barrel jack  | 1   | 5.5×2.1mm standard                  |
| LM2596 / MP1584 buck module        | 1   | pre-set to 5V or trim the pot       |
| INMP441 I2S mic module             | 1   | if mic on this unit                 |
| u.FL → SMA pigtail + 2.4GHz antenna| 1   | for external antenna option        |
| Enclosure + standoffs              | 1   | 3D-printed or generic plastic box  |
