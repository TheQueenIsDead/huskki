#include <SPI.h>
#include <mcp2515.h>

#define CAN_CS_PIN   9
#define CAN_SPEED    CAN_500KBPS     // change if needed
#define CAN_CLOCK    MCP_16MHZ       // use MCP_8MHZ if your board has 8 MHz

MCP2515 mcp2515(CAN_CS_PIN);
struct can_frame rx;

static inline void printFrame(const struct can_frame &f) {
  unsigned long ms = millis();
  bool ext = f.can_id & CAN_EFF_FLAG;         // extended frame?
  bool rtr = f.can_id & CAN_RTR_FLAG;         // remote request?
  uint32_t id = ext ? (f.can_id & CAN_EFF_MASK)
                    : (f.can_id & CAN_SFF_MASK);

  Serial.print(ms);
  Serial.print("  ");
  Serial.print(ext ? 'X' : 'S');              // X=29-bit, S=11-bit
  Serial.print(' ');
  Serial.print(rtr ? "RTR " : "DAT ");
  Serial.print("id=0x"); Serial.print(id, HEX);
  Serial.print(" dlc="); Serial.print(f.can_dlc);
  Serial.print(" data=");
  for (uint8_t i = 0; i < f.can_dlc; i++) {
    if (i) Serial.print(' ');
    if (f.data[i] < 0x10) Serial.print('0');
    Serial.print(f.data[i], HEX);
  }
  Serial.println();
}

void setup() {
  Serial.begin(115200);
  while (!Serial) {}

  pinMode(10, OUTPUT);          // keep AVR SPI in master mode
  pinMode(CAN_CS_PIN, OUTPUT);

  mcp2515.reset();
  mcp2515.setBitrate(CAN_SPEED, CAN_CLOCK);

  // ---- Accept EVERYTHING on both RX buffers ----
  mcp2515.setConfigMode();

  // RXB0 : accept all 11-bit (standard)
  mcp2515.setFilterMask(MCP2515::MASK0, false, 0x000);      // std mask = 0 -> don't care
  mcp2515.setFilter(MCP2515::RXF0,    false, 0x000);
  mcp2515.setFilter(MCP2515::RXF1,    false, 0x000);

  // RXB1 : accept all 29-bit (extended)
  mcp2515.setFilterMask(MCP2515::MASK1, true, 0x00000000);  // ext mask = 0 -> don't care
  mcp2515.setFilter(MCP2515::RXF2,    true, 0x00000000);
  mcp2515.setFilter(MCP2515::RXF3,    true, 0x00000000);
  mcp2515.setFilter(MCP2515::RXF4,    true, 0x00000000);
  mcp2515.setFilter(MCP2515::RXF5,    true, 0x00000000);

  mcp2515.setNormalMode();

  Serial.println(F("MCP2515 sniffing… (all frames)"));
}

void loop() {
  // Drain the receive FIFO as fast as possible
  while (mcp2515.readMessage(&rx) == MCP2515::ERROR_OK) {
    printFrame(rx);
  }
  // small yield so we don't starve other tasks
  delay(1);
}