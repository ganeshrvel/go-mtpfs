#import <Foundation/Foundation.h>
#import <ImageCaptureCore/ImageCaptureCore.h>
#import <IOKit/IOKitLib.h>
#include <dlfcn.h>
#include "imagecapture_bridge.h"

// Framework state stays on the main thread; blocking calls run on workers.
// Requests retain native state until completion, including after timeouts.
static const NSUInteger maxPayload = 16 * 1024 * 1024;
static const int commandTimeout = 120;

@interface OMTPReply : NSObject
@property(nonatomic, strong) dispatch_semaphore_t semaphore;
@property(nonatomic, strong) NSData *data;
@property(nonatomic, strong) NSData *response;
@property(nonatomic, copy) NSString *error;
@property(nonatomic) BOOL completed;
- (void)finish:(NSString *)error;
@end

@implementation OMTPReply
- (instancetype)init {
    if ((self = [super init])) _semaphore = dispatch_semaphore_create(0);
    return self;
}
- (void)finish:(NSString *)error {
    if (self.completed) return;
    self.completed = YES;
    self.error = error;
    dispatch_semaphore_signal(self.semaphore);
}
@end

@interface OMTPSession : NSObject <ICDeviceDelegate>
@property(nonatomic, strong) ICCameraDevice *camera;
@property(nonatomic, strong) OMTPReply *pending;
@property(nonatomic, strong) OMTPReply *closeReply;
@property(nonatomic) uint32_t vendor;
@property(nonatomic) uint32_t product;
@property(nonatomic) uint32_t location;
@property(nonatomic) BOOL closed;
@property(nonatomic) BOOL opening;
- (void)invalidate:(NSString *)reason;
- (void)considerDevice:(ICDevice *)device;
@end

// Reuse the browser; replacing it can lose discovery callbacks.
@interface OMTPBrowser : NSObject <ICDeviceBrowserDelegate>
@property(nonatomic, strong) ICDeviceBrowser *browser;
@property(nonatomic, strong) OMTPSession *active;
- (void)open:(OMTPSession *)session;
@end
static OMTPBrowser *sharedBrowser;

@implementation OMTPBrowser
- (void)open:(OMTPSession *)session {
    if (self.active) {
        [session.pending finish:self.active.closed ?
            @"The previous ImageCaptureCore session is still closing; reconnect the phone" :
            @"An ImageCaptureCore connection is already active"];
        return;
    }
    self.active = session;
    if (!self.browser) {
        self.browser = [ICDeviceBrowser new];
        self.browser.delegate = self;
        self.browser.browsedDeviceTypeMask = (ICDeviceTypeMask)
            (ICDeviceTypeMaskCamera | ICDeviceLocationTypeMaskLocal);
        [self.browser start];
    } else {
        for (ICDevice *device in self.browser.devices) [session considerDevice:device];
    }
}
- (void)deviceBrowser:(ICDeviceBrowser *)browser didAddDevice:(ICDevice *)device
          moreComing:(BOOL)moreComing {
    [self.active considerDevice:device];
}
- (void)deviceBrowser:(ICDeviceBrowser *)browser didRemoveDevice:(ICDevice *)device
           moreGoing:(BOOL)moreGoing {
    if (device == self.active.camera)
        [self.active didRemoveDevice:device];
}
@end

@implementation OMTPSession
- (instancetype)init {
    if ((self = [super init])) _closeReply = [OMTPReply new];
    return self;
}
- (void)finishClose:(NSString *)error {
    // Delegates are non-retaining; clear them before releasing the session.
    if (self.camera.delegate == self) self.camera.delegate = nil;
    if (sharedBrowser.active == self) sharedBrowser.active = nil;
    [self.closeReply finish:error];
}
- (void)invalidate:(NSString *)reason {
    if (self.closed) return;
    self.closed = YES;
    [self.pending finish:reason];
    self.pending = nil;
    // Close our client session without disturbing the daemon's USB session.
    if (self.opening) return; // Late open completion must close before reuse.
    if (self.camera.hasOpenSession) {
        if (@available(macOS 10.15, *)) {
            [self.camera requestCloseSessionWithOptions:nil completion:^(NSError *error) {
                dispatch_async(dispatch_get_main_queue(), ^{ [self finishClose:error.localizedDescription]; });
            }];
        }
    } else {
        [self finishClose:nil];
    }
}
- (void)considerDevice:(ICDevice *)device {
    if (self.closed || self.camera || (uint32_t)device.usbVendorID != self.vendor ||
        (uint32_t)device.usbProductID != self.product || (uint32_t)device.usbLocationID != self.location ||
        ![device isKindOfClass:ICCameraDevice.class]) return;
    self.camera = (ICCameraDevice *)device;
    device.delegate = self;
    if (![device.capabilities containsObject:ICCameraDeviceCanAcceptPTPCommands]) {
        [self invalidate:@"The camera service does not support PTP commands for this device"];
        return;
    }
    if (@available(macOS 10.15, *)) {
        // Undocumented option: skip camera cataloging when available.
        NSString * __unsafe_unretained *tethering =
            (NSString * __unsafe_unretained *)dlsym(RTLD_DEFAULT, "ICEnumerationPrioritizeTethering");
        NSDictionary *options = tethering && *tethering ? @{*tethering: @YES} : nil;
        self.opening = YES;
        [device requestOpenSessionWithOptions:options completion:^(NSError *error) {
            dispatch_async(dispatch_get_main_queue(), ^{
                self.opening = NO;
                if (self.closed) {
                    if (device.hasOpenSession) {
                        [device requestCloseSessionWithOptions:nil completion:^(NSError *closeError) {
                            dispatch_async(dispatch_get_main_queue(), ^{ [self finishClose:closeError.localizedDescription]; });
                        }];
                    } else {
                        [self finishClose:nil];
                    }
                    return;
                }
                [self.pending finish:error.localizedDescription];
                self.pending = nil;
            });
        }];
    }
}
- (void)device:(ICDevice *)device didOpenSessionWithError:(NSError *)error {
    // Handled by the open completion.
}
- (void)device:(ICDevice *)device didCloseSessionWithError:(NSError *)error {
    if (!self.closed) [self invalidate:error.localizedDescription ?: @"ImageCaptureCore session closed"];
}
- (void)didRemoveDevice:(ICDevice *)device {
    [self invalidate:@"Phone disconnected from ImageCaptureCore; reconnect the phone"];
    [self finishClose:nil];
}
@end

static NSString *property(io_service_t service, CFStringRef name) {
    id value = CFBridgingRelease(IORegistryEntryCreateCFProperty(service, name, NULL, 0));
    return [value isKindOfClass:NSString.class] ? value : @"";
}

char *omtp_ic_devices(char **error) {
    @autoreleasepool {
        io_iterator_t iterator = 0;
        kern_return_t result = IOServiceGetMatchingServices(kIOMasterPortDefault,
            IOServiceMatching("IOUSBHostInterface"), &iterator);
        if (result != KERN_SUCCESS) {
            *error = strdup("Unable to enumerate USB interfaces");
            return NULL;
        }
        NSMutableDictionary *devices = [NSMutableDictionary new];
        io_service_t service;
        while ((service = IOIteratorNext(iterator))) {
            CFMutableDictionaryRef raw = NULL;
            if (IORegistryEntryCreateCFProperties(service, &raw, NULL, 0) != KERN_SUCCESS) {
                IOObjectRelease(service);
                continue;
            }
            NSDictionary *p = CFBridgingRelease(raw);
            NSString *name = property(service, CFSTR("kUSBString"));
            NSString *owner = property(service, CFSTR("UsbExclusiveOwner"));
            BOOL ptp = [p[@"bInterfaceClass"] intValue] == 6 &&
                       [p[@"bInterfaceSubClass"] intValue] == 1 &&
                       [p[@"bInterfaceProtocol"] intValue] == 1;
            BOOL mtp = [name rangeOfString:@"MTP" options:NSCaseInsensitiveSearch].location != NSNotFound;
            BOOL apple = [owner hasSuffix:@", ptpcamerad"] || [owner hasSuffix:@", PTPCamera"];
            if ((ptp || mtp) && p[@"locationID"] && p[@"idVendor"] && p[@"idProduct"]) {
                NSString *key = [p[@"locationID"] stringValue];
                NSMutableDictionary *d = [@{
                    @"vendor": p[@"idVendor"], @"product": p[@"idProduct"],
                    @"location": p[@"locationID"], @"release": p[@"bcdDevice"] ?: @0,
                    @"manufacturer": property(service, CFSTR("USB Vendor Name")),
                    @"name": property(service, CFSTR("USB Product Name")),
                    @"serial": property(service, CFSTR("USB Serial Number")),
                    @"appleOwned": @(apple)
                } mutableCopy];
                if ([devices[key][@"appleOwned"] boolValue]) d[@"appleOwned"] = @YES;
                devices[key] = d;
            }
            IOObjectRelease(service);
        }
        IOObjectRelease(iterator);
        NSData *json = [NSJSONSerialization dataWithJSONObject:devices.allValues options:0 error:nil];
        return strdup([[NSString alloc] initWithData:json encoding:NSUTF8StringEncoding].UTF8String);
    }
}

void *omtp_ic_open(uint32_t vendor, uint32_t product, uint32_t location, char **error) {
    @autoreleasepool {
        if (@available(macOS 10.15, *)) {
            if (NSThread.isMainThread) {
                *error = strdup("ImageCaptureCore must be opened from a worker thread");
                return NULL;
            }
            OMTPSession *session = [OMTPSession new];
            session.vendor = vendor; session.product = product; session.location = location;
            OMTPReply *reply = [OMTPReply new];
            dispatch_async(dispatch_get_main_queue(), ^{
                session.pending = reply;
                if (!sharedBrowser) sharedBrowser = [OMTPBrowser new];
                [sharedBrowser open:session];
            });
            // Other clients' catalog work can still delay opening.
            if (dispatch_semaphore_wait(reply.semaphore,
                    dispatch_time(DISPATCH_TIME_NOW, (int64_t)commandTimeout * NSEC_PER_SEC))) {
                *error = strdup("ImageCaptureCore session timed out; unlock and reconnect the phone");
                dispatch_async(dispatch_get_main_queue(), ^{ [session invalidate:@"Session open timed out"]; });
                return NULL;
            }
            if (reply.error) {
                *error = strdup(reply.error.UTF8String);
                dispatch_async(dispatch_get_main_queue(), ^{ [session invalidate:reply.error]; });
                return NULL;
            }
            return (__bridge_retained void *)session;
        }
        *error = strdup("ImageCaptureCore MTP forwarding requires macOS 10.15 or later");
        return NULL;
    }
}

static void *copyData(NSData *data) {
    if (!data.length) return NULL;
    void *copy = malloc(data.length);
    if (copy) memcpy(copy, data.bytes, data.length);
    return copy;
}

omtp_ic_reply omtp_ic_command(void *handle, const void *command, size_t commandSize,
                            const void *out, size_t outSize) {
    @autoreleasepool {
        omtp_ic_reply result = {0};
        if (NSThread.isMainThread || commandSize < 12 || commandSize > 32 || outSize > maxPayload) {
            result.error = strdup("Invalid ImageCaptureCore command or calling thread");
            return result;
        }
        OMTPSession *session = (__bridge OMTPSession *)handle;
        OMTPReply *reply = [OMTPReply new];
        // Copy synchronously before dispatch: no Go memory may escape this call.
        NSData *packet = [NSData dataWithBytes:command length:commandSize];
        NSData *payload = outSize ? [NSData dataWithBytes:out length:outSize] : nil;
        dispatch_async(dispatch_get_main_queue(), ^{
            if (session.closed || !session.camera.hasOpenSession) {
                [reply finish:@"ImageCaptureCore session is closed; reconnect the phone"];
                return;
            }
            session.pending = reply;
            if (@available(macOS 10.15, *)) {
                [session.camera requestSendPTPCommand:packet outData:payload
                    completion:^(NSData *data, NSData *response, NSError *error) {
                        dispatch_async(dispatch_get_main_queue(), ^{
                            if (reply.completed) return;
                            NSString *message = error.localizedDescription;
                            if (data.length > maxPayload || response.length > 32)
                                message = @"ImageCaptureCore response exceeded the buffer limit";
                            if (!message) { reply.data = data; reply.response = response; }
                            [reply finish:message];
                            session.pending = nil;
                        });
                    }];
            }
        });
        if (dispatch_semaphore_wait(reply.semaphore,
                dispatch_time(DISPATCH_TIME_NOW, (int64_t)commandTimeout * NSEC_PER_SEC))) {
            result.error = strdup("ImageCaptureCore command timed out; its outcome is unknown. Reconnect the phone before retrying");
            dispatch_async(dispatch_get_main_queue(), ^{ [session invalidate:@"Command timed out"]; });
            return result;
        }
        if (reply.error) { result.error = strdup(reply.error.UTF8String); return result; }
        result.data_size = reply.data.length;
        result.response_size = reply.response.length;
        result.data = copyData(reply.data);
        result.response = copyData(reply.response);
        if ((result.data_size && !result.data) || (result.response_size && !result.response))
            result.error = strdup("Unable to allocate ImageCaptureCore response buffer");
        return result;
    }
}

char *omtp_ic_close(void *handle) {
    @autoreleasepool {
        OMTPSession *session = CFBridgingRelease(handle);
        // Wait for close before allowing the camera to be reused.
        dispatch_async(dispatch_get_main_queue(), ^{ [session invalidate:@"Session closed"]; });
        if (dispatch_semaphore_wait(session.closeReply.semaphore,
                dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC)))
            return strdup("ImageCaptureCore session close timed out; reconnect the phone");
        return session.closeReply.error ? strdup(session.closeReply.error.UTF8String) : NULL;
    }
}

static BOOL mainLoopStopped;
void omtp_ic_run_loop(void) {
    @autoreleasepool {
        mainLoopStopped = NO;
        // Keep CFRunLoop alive even before a browser has been created.
        NSPort *port = [NSMachPort port];
        [[NSRunLoop currentRunLoop] addPort:port forMode:NSDefaultRunLoopMode];
        while (!mainLoopStopped) {
            @autoreleasepool {
                [[NSRunLoop currentRunLoop] runMode:NSDefaultRunLoopMode
                    beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.1]];
            }
        }
        [port invalidate];
    }
}
void omtp_ic_stop_loop(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        mainLoopStopped = YES;
        CFRunLoopStop(CFRunLoopGetMain());
    });
}
