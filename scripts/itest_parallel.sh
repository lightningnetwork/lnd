#!/bin/bash

# Get all the variables.
PROCESSES=$1
TRANCHES=$2
SHUFFLE_SEED=$3

# Here we also shift 3 times and get the rest of our flags to pass on in $@.
shift 3

# Create a variable to hold the final exit code.
exit_code=0

# Run commands using xargs in parallel and capture their PIDs
pids=()
for ((i=0; i<PROCESSES; i++)); do 
    scripts/itest_part.sh $i $TRANCHES $SHUFFLE_SEED $@ &
    pids+=($!)
done


# Wait for the processes created by xargs to finish.
for pid in "${pids[@]}"; do
    wait $pid

    # Once finished, grab its exit code.
    current_exit_code=$?

    # Overwrite the exit code if current itest doesn't return 0.
    if [ $current_exit_code -ne 0 ]; then
        # Only write the exit code of the first failing itest.
        if [ $exit_code -eq 0 ]; then
            exit_code=$current_exit_code
        fi
    fi
done

# Exit with the exit code of the first failing itest or 0.
exit $exit_code
