#ifndef OPENMTP_IMAGECAPTURE_BRIDGE_H
#define OPENMTP_IMAGECAPTURE_BRIDGE_H
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
    void *data;
    size_t data_size;
    void *response;
    size_t response_size;
    char *error;
} omtp_ic_reply;

#ifdef __APPLE__
char *omtp_ic_devices(char **error);
void *omtp_ic_open(uint32_t vendor, uint32_t product, uint32_t location, char **error);
omtp_ic_reply omtp_ic_command(void *session, const void *command, size_t command_size,
                            const void *out, size_t out_size);
char *omtp_ic_close(void *session);
// Main-thread run loop for standalone callers.
void omtp_ic_run_loop(void);
void omtp_ic_stop_loop(void);
#else
// No cameras on other platforms; explicit proxy calls fail.
static inline char *omtp_ic_devices(char **error) {
    (void)error;
    return NULL;
}
static inline void *omtp_ic_open(uint32_t vendor, uint32_t product, uint32_t location, char **error) {
    (void)vendor; (void)product; (void)location;
    *error = strdup("ImageCaptureCore requires macOS");
    return NULL;
}
static inline omtp_ic_reply omtp_ic_command(void *session, const void *command, size_t command_size,
                                           const void *out, size_t out_size) {
    (void)session; (void)command; (void)command_size; (void)out; (void)out_size;
    omtp_ic_reply reply = {0};
    reply.error = strdup("ImageCaptureCore requires macOS");
    return reply;
}
static inline char *omtp_ic_close(void *session) { (void)session; return NULL; }
static inline void omtp_ic_run_loop(void) {}
static inline void omtp_ic_stop_loop(void) {}
#endif
#endif
