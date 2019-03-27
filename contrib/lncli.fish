# fish programmable completion for lncli
# copy to $HOME/.config/fish/completions and restart your shell session

# TODO: provide available arguments for subcommands

complete -c lncli -d "print the version" -l version -s v
complete -c lncli -d "show help" -l help -s h
complete -c lncli -d "if set, lock macaroon to specific IP" -l macaroonip -xf
complete -c lncli -d "anti-replay macaroon validity time in seconds (default: 60)" -l macaroontimeout -xf
complete -c lncli -d "path to macaroon file" -l macaroonpath -x
complete -c lncli -d "disable macaroon authentication" -l no-macaroons
complete -c lncli -d 'the network lnd is running on' -l network -s n -x -a "mainnet testnet regtest simnet"
complete -c lncli -d 'the chain lnd is running on' -l chain -s c -x -a "bitcoin litecoin"
complete -c lncli -d 'path to TLS certificate' -l tlscertpath -x
complete -c lncli -d "path to lnd's base directory" -l lnddir -x
complete -c lncli -d "host:port of ln daemon" -l rpcserver -xf

# get the list of all available subcommands
# in the help section, all subcommands start 
# with 4 spaces
set commands (lncli help | grep '^    ' | grep -v help | sed -e 's/^[[:space:]]*//' | cut -f 1 -d " ")

function __fish_lncli_no_subcommand --description "Test if lncli has yet to be given the subcommand"
    for i in (commandline -opc)
        if contains -- $i $commands
            return 1
        end
    end
    return 0
end

function __fish_lncli_subcommand --description "Test if lncli has been given the subcommand"
    not __fish_lncli_no_subcommand
end

# provide the list of subcommands if the user has not typed in a subcommand yet
complete -c lncli -f -a "$commands" -n '__fish_lncli_no_subcommand'

# if the user has, suggest nothing
complete -c lncli -f -a "" -n '__fish_lncli_subcommand'
