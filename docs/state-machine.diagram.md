# State Machine

The mermaid diagram outlined below represents the various states and events that dictate the functionality of the node.

## Interactive Diagram

```mermaid
stateDiagram-v2
    [*] --> IDLE
    CATCHINGBLOCKS --> RUNNING: RUN
    IDLE --> RUNNING: RUN
    RUNNING --> CATCHINGBLOCKS: CATCHUPBLOCKS
    RUNNING --> IDLE: STOP
```
