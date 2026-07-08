// SPDX-License-Identifier: AGPL-3.0-or-later

// broker — a ZeroMQ ventilator + sink.
//
// It fans TASKS work items out to the workers (PUSH, fair-queued) and collects
// one result per item (PULL), printing which worker handled each. Run under
// Pandion, the workers connect over the encrypted overlay; with two (or more) you
// watch the load split across them. Part of the `zmq-cluster` demo — see
// README.md; no code to write, just `pandion up`.
//
// To split the load reliably we DON'T guess how long workers take to connect —
// each worker checks in on a readiness socket, and the broker only starts
// dispatching once they have. (A plain PUSH that starts sending too early sends
// everything to whichever worker connected first.)
#include <zmq.h>

#include <cstdio>
#include <cstring>
#include <map>
#include <string>

static const int TASKS = 30;

int main() {
    void *ctx = zmq_ctx_new();

    void *vent = zmq_socket(ctx, ZMQ_PUSH); // tasks out
    zmq_bind(vent, "tcp://*:5557");
    void *sink = zmq_socket(ctx, ZMQ_PULL); // results in
    zmq_bind(sink, "tcp://*:5558");
    void *ready = zmq_socket(ctx, ZMQ_PULL); // worker check-ins
    zmq_bind(ready, "tcp://*:5559");

    // Wait for the first worker to check in (up to 30s), then a short grace window
    // to catch the rest — so every connected worker is in the rotation before we
    // dispatch. Works for any number of workers; no hardcoded count.
    printf("[broker] waiting for workers to check in...\n");
    fflush(stdout);
    char rb[64];
    int first_timeout = 30000;
    zmq_setsockopt(ready, ZMQ_RCVTIMEO, &first_timeout, sizeof first_timeout);
    if (zmq_recv(ready, rb, sizeof(rb) - 1, 0) < 0) {
        fprintf(stderr, "[broker] no workers checked in within 30s — aborting\n");
        return 1;
    }
    int workers = 1;
    int grace = 3000;
    zmq_setsockopt(ready, ZMQ_RCVTIMEO, &grace, sizeof grace);
    while (zmq_recv(ready, rb, sizeof(rb) - 1, 0) >= 0) workers++;

    printf("[broker] %d worker(s) ready; dispatching %d tasks over the overlay\n", workers, TASKS);
    fflush(stdout);
    for (int i = 1; i <= TASKS; i++) {
        char task[16];
        int n = snprintf(task, sizeof task, "%d", i);
        zmq_send(vent, task, n, 0);
    }

    std::map<std::string, int> per_worker;
    for (int i = 1; i <= TASKS; i++) {
        char buf[256];
        int n = zmq_recv(sink, buf, sizeof(buf) - 1, 0);
        if (n < 0) continue;
        buf[n] = '\0';
        printf("[broker] result %2d/%d: %s\n", i, TASKS, buf);
        fflush(stdout);
        // buf looks like "task 7 done by worker-2"; tally the trailing name.
        std::string s(buf);
        auto pos = s.rfind(' ');
        if (pos != std::string::npos) per_worker[s.substr(pos + 1)]++;
    }

    printf("[broker] all %d tasks complete. distribution:\n", TASKS);
    for (auto &kv : per_worker)
        printf("[broker]   %s handled %d task(s)\n", kv.first.c_str(), kv.second);
    fflush(stdout);

    zmq_close(vent);
    zmq_close(sink);
    zmq_close(ready);
    zmq_ctx_destroy(ctx);
    return 0;
}
