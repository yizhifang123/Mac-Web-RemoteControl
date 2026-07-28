// Swift-importable shim over libopus (Homebrew). Needed because opus_encoder_ctl is
// variadic and the OPUS_SET_* request macros take arguments — neither imports into
// Swift — so each control we use gets a tiny static inline wrapper here.
#pragma once
#include <opus/opus.h>

static inline int copus_set_bitrate(OpusEncoder *enc, opus_int32 bitrate) {
    return opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
}

// Forward error correction: lets the decoder conceal a lost packet from the next one.
static inline int copus_set_inband_fec(OpusEncoder *enc, opus_int32 enabled) {
    return opus_encoder_ctl(enc, OPUS_SET_INBAND_FEC(enabled));
}

static inline int copus_set_packet_loss_perc(OpusEncoder *enc, opus_int32 perc) {
    return opus_encoder_ctl(enc, OPUS_SET_PACKET_LOSS_PERC(perc));
}
