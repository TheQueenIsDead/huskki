#include <SPI.h>
#include <mcp2515.h>

// ===== Pins / CAN config =====
#define CAN_CS_PIN   9
#define CAN_SPEED    CAN_500KBPS
#define CAN_CLOCK    MCP_16MHZ

// ===== Scan window (tweak as needed) =====
static const uint16_t SCAN_START = 0x600;   // widen to catch non-engine modules
static const uint16_t SCAN_END   = 0x7FF;

static const uint16_t PER_ID_WAIT_MS  = 150;   // wait for a reply
static const uint16_t INTER_ID_GAP_MS = 20;    // gap between IDs

// Try several SA levels (odd numbers = RequestSeed)
static const uint8_t SA_REQ_SUBS[] = { 0x01, 0x03, 0x05 };
static const uint8_t SA_REQ_COUNT   = sizeof(SA_REQ_SUBS);

// ===== Globals =====
MCP2515 mcp2515(CAN_CS_PIN);
struct can_frame rxFrame;

static inline bool sendSF(uint16_t id, const uint8_t* d, uint8_t n) {
  struct can_frame f{};
  f.can_id  = id;
  f.can_dlc = n + 1;
  f.data[0] = n;                 // ISO-TP single frame length
  memcpy(&f.data[1], d, n);
  return mcp2515.sendMessage(&f) == MCP2515::ERROR_OK;
}

static inline bool waitResp(uint16_t expectId, struct can_frame &out, uint16_t ms) {
  unsigned long t0 = millis();
  while (millis() - t0 < ms) {
    if (mcp2515.readMessage(&out) == MCP2515::ERROR_OK) {
      if ((out.can_id & 0x7FF) == expectId) return true;
    }
  }
  return false;
}

static inline void printFrame(const struct can_frame &f) {
  uint16_t id11 = f.can_id & 0x7FF;
  Serial.print(F(" id=0x")); Serial.print(id11, HEX);
  Serial.print(F(" dlc="));  Serial.print(f.can_dlc);
  Serial.print(F(" data="));
  for (uint8_t i = 0; i < f.can_dlc; i++) {
    Serial.print(i ? ' ' : ' ');
    if (f.data[i] < 0x10) Serial.print('0');
    Serial.print(f.data[i], HEX);
  }
  Serial.println();
}

static inline const char* nrcName(uint8_t nrc) {
  switch (nrc) {
    case 0x11: return "ServiceNotSupported";
    case 0x12: return "SubFuncNotSupported";
    case 0x13: return "BadLength";
    case 0x22: return "ConditionsNotCorrect";
    case 0x33: return "SecurityDenied";
    case 0x36: return "TooManyAttempts";
    case 0x37: return "DelayNotExpired";
    case 0x78: return "ResponsePending";
    default:   return "";
  }
}

// Probe one physical ID using several SA request levels
bool probeSecurityAccess(uint16_t reqId) {
  uint16_t rspId = reqId + 8;
  struct can_frame r{};

  for (uint8_t i = 0; i < SA_REQ_COUNT; i++) {
    uint8_t sub = SA_REQ_SUBS[i];
    uint8_t req[] = { 0x27, sub };

    if (!sendSF(reqId, req, sizeof(req))) continue;
    bool got = waitResp(rspId, r, PER_ID_WAIT_MS);
    if (!got) continue;

    // Any reply that mentions SID 0x27 indicates presence
    if (r.can_dlc >= 2 && r.data[1] == 0x67) {
      Serial.print(F("[SEED] req 0x")); Serial.print(reqId, HEX);
      Serial.print(F(" sub=0x")); Serial.print(sub, HEX);
      printFrame(r);
      return true;
    } else if (r.can_dlc >= 3 && r.data[1] == 0x7F && r.data[2] == 0x27) {
      uint8_t nrc = (r.can_dlc >= 4) ? r.data[3] : 0x00;
      Serial.print(F("[NRC ] req 0x")); Serial.print(reqId, HEX);
      Serial.print(F(" sub=0x")); Serial.print(sub, HEX);
      Serial.print(F(" nrc=0x")); Serial.print(nrc, HEX);
      const char* name = nrcName(nrc);
      if (*name) { Serial.print(' '); Serial.print(name); }
      printFrame(r);

      // If server said ResponsePending (0x78), wait a bit longer once
      if (nrc == 0x78) {
        if (waitResp(rspId, r, 300)) {
          Serial.print(F("[POST] req 0x")); Serial.print(reqId, HEX);
          printFrame(r);
        }
      }
      return true; // still marks this ID as present
    }
  }
  return false;
}

void setup() {
  Serial.begin(115200);
  while (!Serial) {}

  pinMode(10, OUTPUT);
  pinMode(CAN_CS_PIN, OUTPUT);

  mcp2515.reset();
  mcp2515.setBitrate(CAN_SPEED, CAN_CLOCK);

  // Receive-all filters (standard 11-bit)
  mcp2515.setConfigMode();
  mcp2515.setFilterMask(MCP2515::MASK0, false, 0x000);
  mcp2515.setFilterMask(MCP2515::MASK1, false, 0x000);
  mcp2515.setFilter(MCP2515::RXF0, false, 0x000);
  mcp2515.setFilter(MCP2515::RXF1, false, 0x000);
  mcp2515.setFilter(MCP2515::RXF2, false, 0x000);
  mcp2515.setFilter(MCP2515::RXF3, false, 0x000);
  mcp2515.setFilter(MCP2515::RXF4, false, 0x000);
  mcp2515.setFilter(MCP2515::RXF5, false, 0x000);
  mcp2515.setNormalMode();

  // Flush any stale frames
  while (mcp2515.readMessage(&rxFrame) == MCP2515::ERROR_OK) {}

  Serial.print(F("SecurityAccess probe: 0x"));
  Serial.print(SCAN_START, HEX);
  Serial.print(F(" → 0x"));
  Serial.println(SCAN_END, HEX);

  for (uint16_t id = SCAN_START; id <= SCAN_END; id++) {
    (void)probeSecurityAccess(id);
    delay(INTER_ID_GAP_MS);
  }
  Serial.println(F("Scan complete."));
}

void loop() {}