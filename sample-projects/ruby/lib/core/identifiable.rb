# typed: strict
# frozen_string_literal: true

module Core
  module Identifiable
    extend T::Sig
    extend T::Helpers

    interface!

    sig { abstract.returns(Integer) }
    def id; end
  end
end
