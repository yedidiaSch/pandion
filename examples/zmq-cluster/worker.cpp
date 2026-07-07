// worker — pulls tasks from the broker's ventilator, "processes" each, and pushes
// a result back to the broker's sink, all over the encrypted overlay.
//
// The broker's endpoints arrive as argv (built from $PANDION_BROKER_IP, which
// Pandion injects on every node — no hardcoded IPs). The worker's own name comes
// from $PANDION_SELF_NAME. It checks in on the broker's readiness socket so the
// broker waits for it before dispatching, then runs until the tasks stop and exits
// cleanly. Part of the `zmq-cluster` demo — see README.md.
#include <zmq.h>

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <unistd.h>

int main(int argc, char **argv) {
    if (argc < 4) {
        fprintf(stderr, "usage: %s <ventilator> <sink> <ready> endpoints\n", argv[0]);
        return 2;
    }
    const char *self = getenv("PANDION_SELF_NAME");
    if (!self || !*self) self = "worker";

    void *ctx = zmq_ctx_new();
    void *in = zmq_socket(ctx, ZMQ_PULL);
    zmq_connect(in, argv[1]); // broker ventilator (tasks in)
    void *out = zmq_socket(ctx, ZMQ_PUSH);
    zmq_connect(out, argv[2]); // broker sink (results out)
    void *reg = zmq_socket(ctx, ZMQ_PUSH);
    zmq_connect(reg, argv[3]); // broker readiness (check-in)

    // Exit a few seconds after the tasks stop arriving — but only once we've seen
    // at least one, so we don't give up while the broker is still warming up.
    int timeout_ms = 3000;
    zmq_setsockopt(in, ZMQ_RCVTIMEO, &timeout_ms, sizeof timeout_ms);

    // Check in: the broker won't dispatch until we (and the other workers) have,
    // so the load actually splits. Sent after the task socket is connected.
    zmq_send(reg, self, strlen(self), 0);
    printf("[%s] checked in with broker; waiting for tasks\n", self);
    fflush(stdout);

    bool started = false;
    for (;;) {
        char buf[64];
        int n = zmq_recv(in, buf, sizeof(buf) - 1, 0);
        if (n < 0) {            // recv timed out
            if (started) break; // broker is done
            continue;           // still waiting for the first task
        }
        started = true;
        buf[n] = '\0';
        usleep(150 * 1000); // simulate work

        char result[256];
        int m = snprintf(result, sizeof result, "task %s done by %s", buf, self);
        zmq_send(out, result, m, 0);
        printf("[%s] processed task %s\n", self, buf);
        fflush(stdout);
    }

    printf("[%s] no more tasks; exiting cleanly\n", self);
    fflush(stdout);
    zmq_close(in);
    zmq_close(out);
    zmq_close(reg);
    zmq_ctx_destroy(ctx);
    return 0;
}
