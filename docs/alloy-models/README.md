# Alloy Models

This folder houses "lightweight formal models" written using the
[Alloy](https://alloytools.org/about.html) model checker and language.

Compared to typical formal methods, Alloy is a _bounded_ model checker,
considered a [lightweight
method](https://en.wikipedia.org/wiki/Formal_methods#:~:text=As%20an%20alternative,Tools.%5B31)
to formal analysis. Lightweight formal methods are easier to use than fully
fledged formal methods as rather than attempting to prove the that a model is
_always_ correct (for all instances), they instead operate on an input of a set
of bounded parameters and iterations. These models can then be used to specify
a model, then use the model checker to find counter examples of a given
assertion. If a counter example can't be found, then the model _may_ be valid.

Alloy in particular is an expressive, human readable language for formal
modeling. It also has a nice visualizer which can show counter examples, aiding
in development and understanding.

Alloy is useful as when used during upfront software design (or even after the
fact), one can create a formal model of a software system to gain better
confidence in the _correctness_ of a system. The model can then be translated
to code. Many times, writing the model (either before or after the code) can
help one to better understand a given software system. Models can also be used
to specify protocols in p2p systems such as Lightning (as it [supports temporal
logic](https://alloytools.org/alloy6.html#:~:text=Meaning%20of%20temporal%20connectives),
which enables creation of state machines and other interactive transcripts),
serving as a complement to a normal prosed based specification.

## How To Learn Alloy?

We recommend the following places to learn Alloy:
  * [The online tutorial for Alloy 4.0](https://alloytools.org/about.html):
     * Even though this is written with Alloy 4.0 (which doesn't support
       temporal logic), the tutorial is very useful, as it introduces
       fundamental concept of Alloy, using an accessible example based on a file
       system.

  * [Alloy Docs](https://alloy.readthedocs.io/en/latest/index.html):
     * This serves as a one stop shop for reference to the Alloy language. It
       explains all the major syntax, tooling, and also how to model time in
       Alloy.

  * [Formal Software Design with Alloy 6](https://haslab.github.io/formal-software-design/index.html): 
     * A more in depth tutorial which uses Alloy 6. This tutorial covers more
       advanced topics such as [event
       reification](https://alloytools.discourse.group/t/modelling-a-state-machine-in-electrum-towards-alloy-6/88).
       This tutorial is also very useful, as it includes examples for how to
       model interactions in a p2p network, such as one peer sending a message
       to another.
