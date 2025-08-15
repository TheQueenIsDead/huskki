#include <SPI.h>
#include <mcp2515.h>

// ===== Pins / CAN config =====
#define CAN_CS_PIN   9
#define CAN_SPEED    CAN_500KBPS
#define CAN_CLOCK    MCP_16MHZ

// ===== UDS constants =====
static const uint8_t SID_TesterPresent = 0x3E;

// ===== Scan config =====
// Typical 11-bit diag IDs live around 0x7E0..0x7EF; feel free to widen.
static const uint16_t SCAN_START = 0x700;
static const uint16_t SCAN_END   = 0x7EF;

static const uint16_t PER_ID_WAIT_MS = 25;   // how long to wait for a reply per ID
static const uint16_t INTER_ID_GAP_MS = 5;   // small gap between IDs

// ===== Globals =====
MCP2515 mcp2515(CAN_CS_PIN);
struct can_frame rxFrame, txFrame;
uint16_t currentId = SCAN_START;
unsigned long lastKick = 0;

// ===== Helpers =====
static inline bool sendTesterPresent(uint16_t reqId) {
  // ISO-TP single frame: [len=2] [SID=0x3E] [suppress=0x00]
  txFrame.can_id  = reqId;       // 11-bit standard ID
  txFrame.can_dlc = 3;
  txFrame.data[0] = 0x02;
  txFrame.data[1] = SID_TesterPresent;
  txFrame.data[2] = 0x00;
  return mcp2515.sendMessage(&txFrame) == MCP2515::ERROR_OK;
}

static inline bool readUntil(uint16_t expectId, uint16_t timeout_ms, struct can_frame &out) {
  unsigned long t0 = millis();
  while ((millis() - t0) < timeout_ms) {
    if (mcp2515.readMessage(&rxFrame) == MCP2515::ERROR_OK) {
      if (rxFrame.can_id == expectId) { out = rxFrame; return true; }
      // else drop it (could be noise or another ECU)
    }
  }
  return false;
}

static inline bool isTpPositiveOrNegResp(const struct can_frame &f) {
  // Expect len >= 3 for both positive (02 7E 00) and negative (03 7F 3E xx) responses.
  if (f.can_dlc < 3) return false;

  // First byte is ISO-TP SF length
  const uint8_t L = f.data[0];
  // Sanity: L shouldn't exceed (dlc - 1)
  if (L > (f.can_dlc - 1)) return false;

  const uint8_t b1 = f.data[1];
  if (b1 == 0x7E) {           // positive response to 0x3E
    // Usually length==2 and data[2]==0x00
    return true;
  } else if (b1 == 0x7F) {    // negative response
    if (f.can_dlc >= 4 && f.data[2] == SID_TesterPresent) return true;
  }
  return false;
}

static inline void printFrame(const struct can_frame &f) {
  Serial.print("  dlc=");
  Serial.print(f.can_dlc);
  Serial.print(" data=");
  for (uint8_t i = 0; i < f.can_dlc; i++) {
    if (i) Serial.print(' ');
    if (f.data[i] < 0x10) Serial.print('0');
    Serial.print(f.data[i], HEX);
  }
  Serial.println();
}

// ===== Setup / Loop =====
void setup() {
  Serial.begin(115200);
  while (!Serial) { /* wait for USB serial on some boards */ }

  pinMode(10, OUTPUT);            // keep AVR SPI in master mode
  pinMode(CAN_CS_PIN, OUTPUT);

  mcp2515.reset();
  mcp2515.setBitrate(CAN_SPEED, CAN_CLOCK);

  // Open up filters to receive everything (standard IDs)
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
  while (mcp2515.readMessage(&rxFrame) == MCP2515::ERROR_OK) { /* drop */ }

  Serial.println(F("UDS TesterPresent scanner starting…"));
  Serial.print(F("Range: 0x"));
  Serial.print(SCAN_START, HEX);
  Serial.print(F(" → 0x"));
  Serial.println(SCAN_END, HEX);

  lastKick = millis();
}

void loop() {
  if (currentId > SCAN_END) {
    // done; restart (or comment this to stop after one pass)
    Serial.println(F("Scan complete. Restarting…"));
    currentId = SCAN_START;
    delay(500);
  }

  // Send TP to current ID
  uint16_t reqId  = currentId;
  uint16_t respId = (uint16_t)(reqId + 8); // standard 11-bit UDS pairing

  if (sendTesterPresent(reqId)) {
    struct can_frame got;
    if (readUntil(respId, PER_ID_WAIT_MS, got) && isTpPositiveOrNegResp(got)) {
      Serial.print(F("[HIT] req 0x"));
      Serial.print(reqId, HEX);
      Serial.print(F("  resp 0x"));
      Serial.print(respId, HEX);
      Serial.println(F("  (TesterPresent)"));
      printFrame(got);
    }
  } else {
    Serial.print(F("[TX FAIL] 0x"));
    Serial.println(reqId, HEX);
  }

  currentId++;
  delay(INTER_ID_GAP_MS);
}
